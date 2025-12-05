package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	stdlog "log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	waProto "go.mau.fi/whatsmeow/binary/proto"
)

const (
	createProbesTableSQL = `
CREATE TABLE IF NOT EXISTS probes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  probe_id INTEGER NOT NULL,
  action TEXT NOT NULL,
  msg_id TEXT NOT NULL,
  send_ts_ns INTEGER NOT NULL,
  receipt_ts_ns INTEGER NOT NULL,
  receipt_type TEXT NOT NULL
);
`
)

// ProbeInfo holds info about a single reaction message we sent.
type ProbeInfo struct {
	ProbeID   int64
	Action    string // "ADD" or "REMOVE"
	SendTSNS  int64
	MessageID string
}

// ProbeRecorder tracks sent probes and writes RTT samples into SQLite.
type ProbeRecorder struct {
	mu          sync.Mutex
	db          *sql.DB
	nextProbeID int64
	sent        map[string]ProbeInfo // key: reaction msg ID
}

func NewProbeRecorder(db *sql.DB) *ProbeRecorder {
	return &ProbeRecorder{
		db:          db,
		nextProbeID: 1,
		sent:        make(map[string]ProbeInfo),
	}
}

// RegisterSend is called right after a reaction is sent.
func (pr *ProbeRecorder) RegisterSend(msgID types.MessageID, action string, t time.Time) int64 {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	idStr := string(msgID)
	probeID := pr.nextProbeID
	pr.nextProbeID++

	info := ProbeInfo{
		ProbeID:   probeID,
		Action:    action,
		SendTSNS:  t.UnixNano(),
		MessageID: idStr,
	}
	pr.sent[idStr] = info

	return probeID
}

// HandleReceipt is called for every receipt event.
func (pr *ProbeRecorder) HandleReceipt(rcpt *events.Receipt) {
	for _, msgID := range rcpt.MessageIDs {
		idStr := string(msgID)

		pr.mu.Lock()
		info, ok := pr.sent[idStr]
		pr.mu.Unlock()

		if !ok {
			// Not a reaction we sent – ignore.
			continue
		}

		receiptNS := rcpt.Timestamp.UnixNano()
		receiptType := fmt.Sprint(rcpt.Type)

		_, err := pr.db.Exec(
			`INSERT INTO probes (probe_id, action, msg_id, send_ts_ns, receipt_ts_ns, receipt_type)
         VALUES (?, ?, ?, ?, ?, ?)`,
			info.ProbeID,
			info.Action,
			info.MessageID,
			info.SendTSNS,
			receiptNS,
			receiptType,
		)
		if err != nil {
			stdlog.Printf("recordReceipt: db insert error: %v (msgID=%s, type=%s)", err, idStr, receiptType)
			continue
		}

		stdlog.Printf("recordReceipt: probe_id=%d action=%s msg_id=%s type=%s rtt_ms=%.3f",
			info.ProbeID,
			info.Action,
			info.MessageID,
			receiptType,
			float64(receiptNS-info.SendTSNS)/1e6,
		)
	}
}

// initProbeDB opens the measurement DB and ensures schema exists.
func initProbeDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(createProbesTableSQL); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ensureBaseMessageID either uses the provided ID or sends a fresh text
// message and returns its ID.
func ensureBaseMessageID(
	ctx context.Context,
	client *whatsmeow.Client,
	toJID types.JID,
	provided string,
) (string, error) {
	if provided != "" {
		stdlog.Printf("Using provided base message ID: %s", provided)
		return provided, nil
	}

	stdlog.Printf("No -base-msg-id provided, sending a base message...")
	msg := &waProto.Message{
		Conversation: proto.String("Hello World!"),
	}

	resp, err := client.SendMessage(ctx, toJID, msg)
	if err != nil {
		return "", fmt.Errorf("sending base message failed: %w", err)
	}

	baseID := string(resp.ID)
	stdlog.Printf("Base message sent: ID=%s", baseID)
	return baseID, nil
}

// sendReaction sends a reaction (add or remove) to the given base message.
// Pass emoji to add/update a reaction, or empty string "" to remove it.
func sendReaction(
	ctx context.Context,
	client *whatsmeow.Client,
	toJID types.JID,
	baseMsgID string,
	emoji string,
) (types.MessageID, error) {
	nowMs := time.Now().UnixNano() / int64(time.Millisecond)

	msg := &waProto.Message{
		ReactionMessage: &waProto.ReactionMessage{
			Key: &waProto.MessageKey{
				ID:        proto.String(baseMsgID),
				RemoteJID: proto.String(toJID.String()),
				FromMe:    proto.Bool(true),
			},
			Text:              proto.String(emoji),
			SenderTimestampMS: proto.Int64(nowMs),
		},
	}

	resp, err := client.SendMessage(ctx, toJID, msg)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func main() {
	// ---------- Flags ----------
	clientDBPath := flag.String("db", "client.db", "Path to Whatsmeow client SQLite database")
	probeDBPath := flag.String("probe-db", "probes.db", "Path to probe measurements SQLite database")
	toStr := flag.String("to", "", "Target JID (e.g. 1234567890@s.whatsapp.net)")
	rateMs := flag.Int("rate", 3000, "Probe interval in milliseconds")
	emoji := flag.String("emoji", "🙂", "Reaction emoji to use")
	baseMsgIDFlag := flag.String("base-msg-id", "", "Existing base message ID to react to (optional)")
	maxRuntimeFlag := flag.String("max-runtime", "", "Maximum runtime (e.g. 6h, 1h30m). Empty = no limit")

	flag.Parse()

	if *toStr == "" {
		stdlog.Fatal("'-to' is required, e.g. -to 919389442656@s.whatsapp.net")
	}

	var maxRuntime time.Duration
	var err error
	if *maxRuntimeFlag != "" {
		maxRuntime, err = time.ParseDuration(*maxRuntimeFlag)
		if err != nil {
			stdlog.Fatalf("invalid -max-runtime value %q: %v", *maxRuntimeFlag, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT/SIGTERM so we can cleanly disconnect.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		stdlog.Printf("Signal received, shutting down...")
		cancel()
	}()

	// ---------- Whatsmeow client setup ----------
	stdlog.Printf("Opening Whatsmeow store: %s", *clientDBPath)
	logger := waLog.Stdout("whatsmeow", "INFO", true)

	storeContainer, err := sqlstore.New(
		ctx,
		"sqlite3",
		fmt.Sprintf("file:%s?_foreign_keys=on", *clientDBPath),
		logger,
	)
	if err != nil {
		stdlog.Fatalf("failed to create store: %v", err)
	}

	device, err := storeContainer.GetFirstDevice(ctx)
	if err != nil {
		stdlog.Fatalf("failed to get device: %v", err)
	}

	client := whatsmeow.NewClient(device, logger)
	defer client.Disconnect()

	// ---------- Probe DB ----------
	probeDB, err := initProbeDB(*probeDBPath)
	if err != nil {
		stdlog.Fatalf("failed to open probe DB: %v", err)
	}
	defer probeDB.Close()

	recorder := NewProbeRecorder(probeDB)

	// ---------- Event handler ----------
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Receipt:
			recorder.HandleReceipt(v)
			// We intentionally ignore other event types.
		}
	})

	// ---------- Connect ----------
	stdlog.Printf("Connecting WhatsApp client...")
	if err := client.Connect(); err != nil {
		stdlog.Fatalf("failed to connect: %v", err)
	}
	stdlog.Printf("Connected.")

	// ---------- Parse JID ----------
	toJID, err := types.ParseJID(*toStr)
	if err != nil {
		stdlog.Fatalf("invalid -to JID %q: %v", *toStr, err)
	}

	// ---------- Base message ----------
	baseMsgID, err := ensureBaseMessageID(ctx, client, toJID, *baseMsgIDFlag)
	if err != nil {
		stdlog.Fatalf("failed to obtain base message ID: %v", err)
	}
	stdlog.Printf("Base message ID for reactions: %s", baseMsgID)

	// ---------- Probe loop ----------
	interval := time.Duration(*rateMs) * time.Millisecond
	stdlog.Printf("Starting probe loop: interval=%s, emoji=%q (alternating ADD/REMOVE)", interval, *emoji)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var runtimeTimer <-chan time.Time
	if maxRuntime > 0 {
		rt := time.NewTimer(maxRuntime)
		defer rt.Stop()
		runtimeTimer = rt.C
		stdlog.Printf("Max runtime: %s", maxRuntime)
	}

	// Toggle between ADD and REMOVE
	nextIsAdd := true

	for {
		select {
		case <-ctx.Done():
			stdlog.Printf("Context cancelled, stopping probe loop.")
			return

		case <-runtimeTimer:
			stdlog.Printf("Max runtime reached, stopping probe loop.")
			return

		case t := <-ticker.C:
			var action string
			var reactionEmoji string

			if nextIsAdd {
				action = "ADD"
				reactionEmoji = *emoji
			} else {
				action = "REMOVE"
				reactionEmoji = "" // Empty string removes reaction
			}

			msgID, err := sendReaction(ctx, client, toJID, baseMsgID, reactionEmoji)
			if err != nil {
				stdlog.Printf("sendReaction error (action=%s): %v", action, err)
				continue
			}
			probeID := recorder.RegisterSend(msgID, action, t)
			stdlog.Printf("Sent reaction probe_id=%d action=%s reaction_msg_id=%s", probeID, action, string(msgID))

			nextIsAdd = !nextIsAdd // Toggle for next iteration
		}
	}
}
