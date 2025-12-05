package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// ProbeState keeps in-memory state needed to compute RTTs.
type ProbeState struct {
	db *sql.DB

	emoji string

	// sentTimes / sentActions are keyed by reaction message ID (string).
	sentTimes   sync.Map // map[string]time.Time
	sentActions sync.Map // map[string]string
}

func (ps *ProbeState) initSchema(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS probe_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	msg_id TEXT NOT NULL,
	action TEXT NOT NULL,
	emoji TEXT NOT NULL,
	sent_at_ms INTEGER NOT NULL,
	receipt_ts_ms INTEGER NOT NULL,
	rtt_ms INTEGER NOT NULL,
	receipt_type TEXT NOT NULL
);
`
	_, err := ps.db.ExecContext(ctx, schema)
	return err
}

func (ps *ProbeState) handleReceipt(ctx context.Context, rcpt *events.Receipt) {
	for _, mid := range rcpt.MessageIDs {
		idStr := mid

		// We only care about reaction messages that *we* sent.
		v, ok := ps.sentTimes.Load(idStr)
		if !ok {
			continue
		}
		sentAt := v.(time.Time)

		actVal, ok := ps.sentActions.Load(idStr)
		if !ok {
			// Should not happen, but don't crash if it does.
			actVal = "UNKNOWN"
		}
		action := actVal.(string)

		rtt := rcpt.Timestamp.Sub(sentAt)
		rttMs := rtt.Milliseconds()

		_, err := ps.db.ExecContext(
			ctx,
			`INSERT INTO probe_events
			 (msg_id, action, emoji, sent_at_ms, receipt_ts_ms, rtt_ms, receipt_type)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			idStr,
			action,
			ps.emoji,
			sentAt.UnixMilli(),
			rcpt.Timestamp.UnixMilli(),
			rttMs,
			string(rcpt.Type),
		)
		if err != nil {
			log.Printf("ERROR: inserting probe_event for %s: %v", idStr, err)
		} else {
			log.Printf("RTT recorded: msg_id=%s action=%s type=%s rtt_ms=%d",
				idStr, action, string(rcpt.Type), rttMs)
		}

		// Drop from in-memory maps to keep memory bounded.
		ps.sentTimes.Delete(idStr)
		ps.sentActions.Delete(idStr)
	}
}

func main() {
	dbPath := flag.String("db", "client.db", "Whatsmeow client database (SQLite file)")
	probeDBPath := flag.String("probe-db", "probes.db", "Probe results database (SQLite file)")
	toJIDStr := flag.String("to", "", "Target JID (e.g. 919xxxxxxxxx@s.whatsapp.net)")
	rateMs := flag.Int("rate", 3000, "Interval between reactions in milliseconds")
	emoji := flag.String("emoji", "🙂", "Emoji to use for reactions")
	baseIDFlag := flag.String("base-id", "", "Existing base message ID to react to (optional)")
	maxRuntime := flag.Duration("max-runtime", 0, "Max runtime (e.g. 6h, 24h). 0 means run forever.")

	flag.Parse()

	if *toJIDStr == "" {
		fmt.Fprintln(os.Stderr, "ERROR: -to is required, e.g. -to 919389442656@s.whatsapp.net")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *maxRuntime > 0 {
		go func() {
			log.Printf("Max runtime set to %s", maxRuntime.String())
			timer := time.NewTimer(*maxRuntime)
			<-timer.C
			log.Printf("Max runtime reached, shutting down...")
			cancel()
		}()
	}

	// Whatsmeow DB (existing client)
	dbLog := waLog.Stdout("Database", "INFO", true)
	dbDSN := fmt.Sprintf("file:%s?_foreign_keys=on", *dbPath)
	container, err := sqlstore.New(ctx, "sqlite3", dbDSN, dbLog)
	if err != nil {
		log.Fatalf("Failed to open Whatsmeow DB: %v", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("Failed to get device: %v", err)
	}
	if deviceStore == nil {
		log.Fatalf("No device found. You must pair/login with whatsmeow first.")
	}

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	// Probe DB
	probeDSN := fmt.Sprintf("file:%s?_foreign_keys=on", *probeDBPath)
	probeDB, err := sql.Open("sqlite3", probeDSN)
	if err != nil {
		log.Fatalf("Failed to open probe DB: %v", err)
	}
	defer probeDB.Close()

	state := &ProbeState{
		db:    probeDB,
		emoji: *emoji,
	}

	if err := state.initSchema(ctx); err != nil {
		log.Fatalf("Failed to init probe DB schema: %v", err)
	}

	// Event handler
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Receipt:
			state.handleReceipt(ctx, v)
		case *events.Connected:
			log.Printf("Connected to WhatsApp")
		}
	})

	if err := client.Connect(); err != nil {
		log.Fatalf("Failed to connect client: %v", err)
	}

	targetJID, err := types.ParseJID(*toJIDStr)
	if err != nil {
		log.Fatalf("Invalid -to JID: %v", err)
	}

	// Decide base message ID:
	var baseMsgID types.MessageID
	if *baseIDFlag != "" {
		baseMsgID = types.MessageID(*baseIDFlag)
		log.Printf("Using existing base message ID: %s", baseMsgID)
	} else {
		// Send a simple text message once and use it as base.
		msg := &waE2E.Message{
			Conversation: proto.String("Hello World!"),
		}
		resp, err := client.SendMessage(ctx, targetJID, msg)
		if err != nil {
			log.Fatalf("Failed to send base message: %v", err)
		}
		baseMsgID = resp.ID
		log.Printf("Sent base message. ID: %s", baseMsgID)
	}

	log.Printf("Base message ID for reactions: %s", baseMsgID)
	log.Printf("Starting probe loop: interval=%dms, emoji=%q", *rateMs, *emoji)

	// Reaction loop
	ticker := time.NewTicker(time.Duration(*rateMs) * time.Millisecond)
	defer ticker.Stop()

	nextAdd := true

	for {
		select {
		case <-ctx.Done():
			log.Printf("Context cancelled, stopping probe loop.")
			return

		case t := <-ticker.C:
			var action string
			var reactionMsg *waE2E.Message

			if nextAdd {
				// Add reaction
				action = "ADD"
				reactionMsg = client.BuildReaction(
					targetJID,
					types.EmptyJID,
					baseMsgID,
					*emoji,
				)
			} else {
				// Remove reaction: send empty reaction
				action = "REMOVE"
				reactionMsg = client.BuildReaction(
					targetJID,
					types.EmptyJID,
					baseMsgID,
					"",
				)
			}

			sendTime := time.Now()
			resp, err := client.SendMessage(ctx, targetJID, reactionMsg)
			if err != nil {
				log.Printf("ERROR: sending reaction (%s) failed: %v", action, err)
				continue
			}

			msgID := resp.ID
			state.sentTimes.Store(msgID, sendTime)
			state.sentActions.Store(msgID, action)

			log.Printf("[%s] Reaction %s sent: reaction_msg_id=%s base_msg_id=%s",
				t.Format(time.RFC3339),
				action,
				msgID,
				baseMsgID,
			)

			nextAdd = !nextAdd
		}
	}
}
