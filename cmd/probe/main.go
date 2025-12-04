package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/whatsapp"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// pendingInfo tracks a reaction message we sent (for RTT computation).
type pendingInfo struct {
	BaseMsgID string
	Phase     string // "add" or "remove"
	Emoji     string
	SendTime  time.Time
}

var (
	pendingMu sync.Mutex
	pending   = make(map[string]pendingInfo)
)

func main() {
	// Flags
	dbPath := flag.String("db", "client.db", "Path to WhatsApp client database (same as mdtest)")
	probeDBPath := flag.String("probe-db", "probes.db", "Path to SQLite database for probe measurements")
	toJIDStr := flag.String("to", "", "Target JID, e.g. 919xxxxxxx@s.whatsapp.net")
	baseMsgID := flag.String("msgid", "", "Existing message ID to react to")
	emoji := flag.String("emoji", "🙂", "Emoji to use for reactions")
	rateMS := flag.Int("rate", 3000, "Probe interval in milliseconds (between reaction messages)")
	maxRuntimeStr := flag.String("max-runtime", "", "Maximum runtime (e.g. 10m, 1h, 6h). Empty = unlimited")

	flag.Parse()

	if *toJIDStr == "" {
		fmt.Fprintln(os.Stderr, "-to is required, e.g. -to 919389442656@s.whatsapp.net")
		os.Exit(1)
	}
	if *baseMsgID == "" {
		fmt.Fprintln(os.Stderr, "-msgid is required (ID of existing message to react to)")
		os.Exit(1)
	}

	jid, err := types.ParseJID(*toJIDStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid -to JID: %v\n", err)
		os.Exit(1)
	}

	var maxRuntime time.Duration
	if *maxRuntimeStr != "" {
		maxRuntime, err = time.ParseDuration(*maxRuntimeStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid -max-runtime value: %v\n", err)
			os.Exit(1)
		}
	}

	rate := time.Duration(*rateMS) * time.Millisecond

	// Logging
	mainLog := waLog.Stdout("Main", "INFO", true)
	dbLog := waLog.Stdout("Database", "INFO", true)
	clientLog := waLog.Stdout("Client", "INFO", true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C / SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		mainLog.Info("signal", "Interrupt received, shutting down")
		cancel()
	}()

	// WhatsApp client store
	// NOTE: your whatsmeow version expects a context for New(..).
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+*dbPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		mainLog.Error("sqlstore", "Failed to open WhatsApp DB", "err", err)
		os.Exit(1)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		mainLog.Error("device", "Failed to get device", "err", err)
		os.Exit(1)
	}

	client := whatsmeow.NewClient(device, clientLog)

	// Probe measurement DB
	probeDB, err := sql.Open("sqlite3", *probeDBPath)
	if err != nil {
		mainLog.Error("probe-db", "Failed to open probe DB", "err", err)
		os.Exit(1)
	}
	defer probeDB.Close()

	if err := initProbeSchema(probeDB); err != nil {
		mainLog.Error("probe-db", "Failed to init schema", "err", err)
		os.Exit(1)
	}

	// Event handler: record receipts that match reactions we sent
	client.AddEventHandler(func(ev interface{}) {
		switch v := ev.(type) {
		case *events.Receipt:
			handleReceiptEvent(mainLog, probeDB, v)
		}
	})

	// Connect
	if err := client.Connect(); err != nil {
		mainLog.Error("client", "Failed to connect", "err", err)
		os.Exit(1)
	}
	mainLog.Info("client", "Connected")

	// Start probe loop in background
	go runProbeLoop(ctx, mainLog, client, probeDB, jid, *baseMsgID, *emoji, rate, maxRuntime)

	// Block until context cancelled
	<-ctx.Done()
	mainLog.Info("main", "Exiting")
}

// initProbeSchema ensures the measurements table exists.
func initProbeSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS probe_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    base_msg_id TEXT NOT NULL,
    reaction_msg_id TEXT NOT NULL,
    phase TEXT NOT NULL,          -- 'add' or 'remove'
    emoji TEXT NOT NULL,
    receipt_type TEXT NOT NULL,   -- e.g. 'sender', 'delivered', 'read', 'inactive'
    send_unix_ms INTEGER NOT NULL,
    receipt_unix_ms INTEGER NOT NULL,
    rtt_ms INTEGER NOT NULL
);
`
	_, err := db.Exec(schema)
	return err
}

// runProbeLoop periodically sends add/remove reactions to baseMsgID.
func runProbeLoop(
	ctx context.Context,
	log waLog.Logger,
	client *whatsmeow.Client,
	probeDB *sql.DB,
	to types.JID,
	baseMsgID string,
	emoji string,
	interval time.Duration,
	maxRuntime time.Duration,
) {
	log.Info("probe", "Starting probe loop",
		"interval", interval.String(),
		"maxRuntime", maxRuntime.String(),
		"baseMsgID", baseMsgID,
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	startTime := time.Now()
	addNext := true
	cycle := 0

	for {
		if maxRuntime > 0 && time.Since(startTime) >= maxRuntime {
			log.Info("probe", "Max runtime reached, stopping probe loop")
			return
		}

		select {
		case <-ctx.Done():
			log.Info("probe", "Context cancelled, stopping probe loop")
			return
		case <-ticker.C:
			cycle++
			phase := "add"
			doAdd := true
			if !addNext {
				phase = "remove"
				doAdd = false
			}
			addNext = !addNext

			reactionMsgID, sendTime, err := sendReaction(ctx, client, to, baseMsgID, emoji, doAdd)
			if err != nil {
				log.Error("probe", "Failed to send reaction",
					"cycle", cycle,
					"phase", phase,
					"err", err)
				continue
			}

			log.Info("probe", "Sent reaction",
				"cycle", cycle,
				"phase", phase,
				"reactionMsgID", reactionMsgID,
				"baseMsgID", baseMsgID)

			// Track pending reaction for RTT measurement
			pendingMu.Lock()
			pending[reactionMsgID] = pendingInfo{
				BaseMsgID: baseMsgID,
				Phase:     phase,
				Emoji:     emoji,
				SendTime:  sendTime,
			}
			pendingMu.Unlock()
		}
	}
}

// sendReaction constructs a ReactionMessage for an existing base message and sends it.
// doAdd=true -> add reaction with emoji
// doAdd=false -> remove reaction (empty text)
func sendReaction(
	ctx context.Context,
	client *whatsmeow.Client,
	to types.JID,
	baseMsgID string,
	emoji string,
	doAdd bool,
) (string, time.Time, error) {
	reaction := &whatsapp.Message_ReactionMessage{
		Key: &whatsapp.MessageKey{
			RemoteJid: proto.String(to.String()),
			FromMe:    proto.Bool(true),
			Id:        proto.String(baseMsgID),
		},
		// Text: emoji for add, empty string for remove
	}
	if doAdd {
		reaction.Text = proto.String(emoji)
	} else {
		reaction.Text = proto.String("")
	}

	msg := &whatsapp.Message{
		ReactionMessage: reaction,
	}

	// You can wrap ctx with timeout if you like
	resp, err := client.SendMessage(ctx, to, msg)
	if err != nil {
		return "", time.Time{}, err
	}
	return resp.ID, resp.Timestamp, nil
}

// handleReceiptEvent stores RTT for any receipts referring to reaction messages we sent.
func handleReceiptEvent(log waLog.Logger, db *sql.DB, evt *events.Receipt) {
	now := time.Now()
	for _, msgID := range evt.MessageIDs {
		pendingMu.Lock()
		info, ok := pending[msgID]
		pendingMu.Unlock()
		if !ok {
			// Not one of our probe reactions; ignore
			continue
		}

		sendMS := info.SendTime.UnixMilli()
		recvMS := now.UnixMilli()
		rtt := recvMS - sendMS

		_, err := db.Exec(
			`INSERT INTO probe_events
			 (base_msg_id, reaction_msg_id, phase, emoji, receipt_type, send_unix_ms, receipt_unix_ms, rtt_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			info.BaseMsgID,
			msgID,
			info.Phase,
			info.Emoji,
			string(evt.Type),
			sendMS,
			recvMS,
			rtt,
		)
		if err != nil {
			log.Error("probe-db", "Failed to insert probe_events row", "err", err)
			continue
		}

		log.Debug("probe", "Stored receipt",
			"msgID", msgID,
			"phase", info.Phase,
			"type", evt.Type,
			"rtt_ms", rtt)
	}
}
