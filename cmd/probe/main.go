package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	walog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// probe record in DB: one row per reaction message we send
type probeRecord struct {
	TargetJID      string
	TargetMsgID    string
	ReactionMsgID  string
	Action         string
	Emoji          string
	SendTSUnixNano int64
}

func main() {
	var (
		storeDBPath   string
		probeDBPath   string
		toJIDStr      string
		targetMsgID   string
		emoji         string
		rateMillis    int
		maxProbes     int
	)

	flag.StringVar(&storeDBPath, "db", "store.db", "Whatsmeow store DB path (sqlite)")
	flag.StringVar(&probeDBPath, "probe-db", "probes.db", "SQLite DB path for probe results")
	flag.StringVar(&toJIDStr, "to", "", "Target JID (e.g. 91989xxxxx@s.whatsapp.net)")
	flag.StringVar(&targetMsgID, "target-msg-id", "", "Existing message ID to react to")
	flag.StringVar(&emoji, "emoji", "👍", "Emoji to use for reaction probes")
	flag.IntVar(&rateMillis, "rate", 3000, "Interval between probes in milliseconds")
	flag.IntVar(&maxProbes, "max-probes", 0, "Maximum number of reaction probes (0 = infinite)")
	flag.Parse()

	if toJIDStr == "" {
		log.Fatal("-to is required")
	}
	if targetMsgID == "" {
		log.Fatal("-target-msg-id is required (ID of an existing message to react to)")
	}
	if emoji == "" {
		log.Fatal("-emoji cannot be empty (use something like \"👍\")")
	}

	// ----- init probe DB -----
	probeDB, err := sql.Open("sqlite3", probeDBPath)
	if err != nil {
		log.Fatalf("failed to open probe DB: %v", err)
	}
	defer probeDB.Close()

	if err := initProbeDB(probeDB); err != nil {
		log.Fatalf("failed to init probe DB schema: %v", err)
	}

	// ----- init WhatsMeow client -----
	container, err := sqlstore.New("sqlite3", storeDBPath, nil)
	if err != nil {
		log.Fatalf("failed to init sqlstore: %v", err)
	}

	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		log.Fatalf("failed to get device: %v", err)
	}

	logger := walog.Stdout("Main", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, logger)

	// ----- receipt handler -----
	var (
		probeDBMu sync.Mutex // serialize DB writes for safety
	)

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Receipt:
			handleReceiptEvent(probeDB, &probeDBMu, v)
		}
	})

	// ----- connect -----
	if client.Store.ID == nil {
		log.Fatal("no device ID in store; you must register / login this device first")
	}

	err = client.Connect()
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	log.Printf("[MAIN] connected as %s", client.Store.ID.String())

	// ----- start probe loop -----
	toJID, err := types.ParseJID(toJIDStr)
	if err != nil {
		log.Fatalf("invalid -to JID: %v", err)
	}

	go func() {
		runProbeLoop(client, probeDB, &probeDBMu, toJID, types.MessageID(targetMsgID), emoji, rateMillis, maxProbes)
	}()

	// ----- wait for Ctrl+C -----
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	log.Println("[MAIN] shutting down...")
	client.Disconnect()
}

// initProbeDB creates the schema we actually need for RTT analysis
func initProbeDB(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS probes (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	target_jid      TEXT    NOT NULL,
	target_msg_id   TEXT    NOT NULL,
	reaction_msg_id TEXT    NOT NULL UNIQUE,
	action          TEXT    NOT NULL,
	emoji           TEXT    NOT NULL,
	send_ts_ns      INTEGER NOT NULL,
	receipt_type    TEXT,
	receipt_ts_ns   INTEGER
);
`
	_, err := db.Exec(schema)
	return err
}

// runProbeLoop periodically sends add/remove reactions to a fixed target message
func runProbeLoop(
	client *whatsmeow.Client,
	db *sql.DB,
	dbMu *sync.Mutex,
	targetJID types.JID,
	targetMsgID types.MessageID,
	emoji string,
	rateMillis int,
	maxProbes int,
) {
	ticker := time.NewTicker(time.Duration(rateMillis) * time.Millisecond)
	defer ticker.Stop()

	ctx := context.Background()

	add := true
	sentCount := 0

	log.Printf("[PROBE] starting probe loop: targetJID=%s targetMsgID=%s emoji=%q rate=%dms maxProbes=%d",
		targetJID.String(), string(targetMsgID), emoji, rateMillis, maxProbes)

	for {
		if maxProbes > 0 && sentCount >= maxProbes {
			log.Printf("[PROBE] reached maxProbes=%d, stopping loop", maxProbes)
			return
		}

		<-ticker.C

		action := "add"
		text := emoji
		if !add {
			action = "remove"
			text = "" // empty text = remove reaction
		}

		resp, err := sendReaction(ctx, client, targetJID, targetMsgID, text)
		if err != nil {
			log.Printf("[PROBE] sendReaction error: %v", err)
			continue
		}

		rec := probeRecord{
			TargetJID:      targetJID.String(),
			TargetMsgID:    string(targetMsgID),
			ReactionMsgID:  string(resp.ID),
			Action:         action,
			Emoji:          text,
			SendTSUnixNano: resp.Timestamp.UnixNano(),
		}

		dbMu.Lock()
		if err := insertProbe(db, &rec); err != nil {
			log.Printf("[PROBE] insertProbe error for %s: %v", rec.ReactionMsgID, err)
		} else {
			log.Printf("[PROBE] %s reaction: targetMsg=%s reactionMsg=%s send_ts_ns=%d",
				action, rec.TargetMsgID, rec.ReactionMsgID, rec.SendTSUnixNano)
		}
		dbMu.Unlock()

		add = !add
		sentCount++
	}
}

// sendReaction sends a reaction to an existing message
func sendReaction(
	ctx context.Context,
	client *whatsmeow.Client,
	targetJID types.JID,
	targetMsgID types.MessageID,
	text string,
) (*whatsmeow.SendResponse, error) {
	// Build ReactionMessage
	r := &waProto.ReactionMessage{
		Key: &waProto.MessageKey{
			RemoteJID: proto.String(targetJID.String()),
			FromMe:    proto.Bool(true),
			ID:        proto.String(string(targetMsgID)),
		},
		// Text: emoji for add; empty string for remove
		Text: proto.String(text),
		// SenderTimestampMS: we can leave nil and let WA fill it
	}

	msg := &waProto.Message{
		ReactionMessage: r,
	}

	resp, err := client.SendMessage(ctx, targetJID, "", msg)
	return resp, err
}

// insertProbe stores a sent reaction message
func insertProbe(db *sql.DB, rec *probeRecord) error {
	_, err := db.Exec(
		`INSERT INTO probes (target_jid, target_msg_id, reaction_msg_id, action, emoji, send_ts_ns)
         VALUES (?, ?, ?, ?, ?, ?)`,
		rec.TargetJID,
		rec.TargetMsgID,
		rec.ReactionMsgID,
		rec.Action,
		rec.Emoji,
		rec.SendTSUnixNano,
	)
	return err
}

// handleReceiptEvent records receipts for reaction messages we know about.
// Any receipt for unknown IDs is silently ignored (no more spam).
func handleReceiptEvent(db *sql.DB, dbMu *sync.Mutex, v *events.Receipt) {
	if len(v.MessageIDs) == 0 {
		return
	}

	// We only care about 'sender' and 'inactive' types for RTT
	typ := string(v.Type)

	dbMu.Lock()
	defer dbMu.Unlock()

	for _, mid := range v.MessageIDs {
		err := updateReceipt(db, string(mid), typ, v.Timestamp)
		if err != nil {
			if err == sql.ErrNoRows {
				// Not one of our probe reaction messages; ignore silently
				continue
			}
			log.Printf("[RECEIPT] updateReceipt error for %s: %v", mid, err)
		}
	}
}

// updateReceipt updates a row for a known reaction message ID
func updateReceipt(db *sql.DB, reactionMsgID string, receiptType string, ts time.Time) error {
	res, err := db.Exec(
		`UPDATE probes
		 SET receipt_type = ?, receipt_ts_ns = ?
		 WHERE reaction_msg_id = ? AND receipt_ts_ns IS NULL`,
		receiptType,
		ts.UnixNano(),
		reactionMsgID,
	)
	if err != nil {
		return err
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if affected == 0 {
		// No row -> behave like sql.ErrNoRows for the caller
		return sql.ErrNoRows
	}
	return nil
}
