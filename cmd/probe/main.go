package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	waE2E "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// ---- CLI flags ----

var (
	dbFile      = flag.String("db", "probe.db", "WhatsMeow SQLite DB file (device store)")
	toJID       = flag.String("to", "", "Target JID (e.g. 9193XXXXXXXX@s.whatsapp.net)")
	outCSV      = flag.String("out", "", "(unused: data now logged to -probe-db SQLite)")
	rateMS      = flag.Int("rate", 3000, "Interval between reaction probe cycles in milliseconds")
	emoji       = flag.String("emoji", "\U0001F44D", "Reaction emoji to use (for ADD phase)")
	probeDBPath = flag.String("probe-db", "probes.db", "SQLite DB file for probe logs")
)

// ---- Probe logging DB layer ----

type ProbeDB struct {
	DB         *sql.DB
	insertStmt *sql.Stmt
	updateStmt *sql.Stmt
}

// initialize/open probe DB and table
func initProbeDB(path string) (*ProbeDB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}

	// Better durability / concurrency
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return nil, err
	}

	create := `
	CREATE TABLE IF NOT EXISTS probes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		probe_id INTEGER,
		phase TEXT,
		base_msg_id TEXT,
		reaction_msg_id TEXT UNIQUE,
		send_ts_ns INTEGER,
		send_iso_utc TEXT,
		receipt_ts_ns INTEGER,
		receipt_iso_utc TEXT,
		rtt_ms REAL,
		receipt_type TEXT,
		note TEXT
	);`
	if _, err := db.Exec(create); err != nil {
		return nil, err
	}

	insertQ := `INSERT OR IGNORE INTO probes (probe_id, phase, base_msg_id, reaction_msg_id, send_ts_ns, send_iso_utc, note) VALUES (?, ?, ?, ?, ?, ?, ?);`
	updateQ := `UPDATE probes SET receipt_ts_ns = ?, receipt_iso_utc = ?, rtt_ms = ?, receipt_type = ? WHERE reaction_msg_id = ?;`

	ins, err := db.Prepare(insertQ)
	if err != nil {
		db.Close()
		return nil, err
	}
	upd, err := db.Prepare(updateQ)
	if err != nil {
		ins.Close()
		db.Close()
		return nil, err
	}

	return &ProbeDB{
		DB:         db,
		insertStmt: ins,
		updateStmt: upd,
	}, nil
}

func (pdb *ProbeDB) Close() {
	if pdb.insertStmt != nil {
		_ = pdb.insertStmt.Close()
	}
	if pdb.updateStmt != nil {
		_ = pdb.updateStmt.Close()
	}
	if pdb.DB != nil {
		_ = pdb.DB.Close()
	}
}

// ns → ISO UTC (matches your Python formatting)
func nsToISO(ns int64) string {
	sec := ns / 1_000_000_000
	nsec := ns % 1_000_000_000
	t := time.Unix(sec, nsec).UTC()
	return fmt.Sprintf("%s.%09dZ", t.Format("2006-01-02T15:04:05"), nsec)
}

// record the send event for a reaction
func (pdb *ProbeDB) recordSend(probeID int, phase string, baseMsgID string, reactionMsgID string, sendTS int64, note string) error {
	iso := nsToISO(sendTS)
	_, err := pdb.insertStmt.Exec(probeID, phase, baseMsgID, reactionMsgID, sendTS, iso, note)
	return err
}

// record the receipt event: updates row, computes RTT
func (pdb *ProbeDB) recordReceipt(reactionMsgID string, receiptTS int64, receiptType string) error {
	iso := nsToISO(receiptTS)

	var sendTS sql.NullInt64
	err := pdb.DB.QueryRow(`SELECT send_ts_ns FROM probes WHERE reaction_msg_id = ?`, reactionMsgID).Scan(&sendTS)

	var rttVal interface{} = nil
	if err == nil && sendTS.Valid {
		rtt := float64(receiptTS-sendTS.Int64) / 1e6
		rttVal = rtt
	}

	_, err2 := pdb.updateStmt.Exec(receiptTS, iso, rttVal, receiptType, reactionMsgID)
	if err2 != nil {
		return err2
	}
	return err
}

// ---- Event handler with DB logging ----

type ProbeTracker struct {
	db  *ProbeDB
	log waLog.Logger
}

func NewProbeTracker(db *ProbeDB, log waLog.Logger) *ProbeTracker {
	return &ProbeTracker{db: db, log: log}
}

func (pt *ProbeTracker) Handler(evt interface{}) {
	switch v := evt.(type) {

	case *events.Message:
		// Minimal logging for debugging
		pt.log.Debugf("[MSG] from %s", v.Info.Chat.String())

	case *events.Receipt:
		now := time.Now().UnixNano()
		for _, mid := range v.MessageIDs {
			err := pt.db.recordReceipt(string(mid), now, string(v.Type))
			if err != nil {
				pt.log.Debugf("recordReceipt error for %s: %v", mid, err)
			}
		}

	default:
		// ignore other events
	}
}

// ---- WhatsApp helper functions ----

// send base "Hello World!" and return message id
func sendHello(ctx context.Context, cli *whatsmeow.Client, jid types.JID) (types.MessageID, error) {
	msg := &waE2E.Message{
		Conversation: proto.String("Hello World!"),
	}

	resp, err := cli.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", fmt.Errorf("SendMessage failed: %w", err)
	}

	fmt.Printf("[SEND] Hello World! -> %s (msgID: %s)\n", jid.String(), resp.ID)
	return types.MessageID(resp.ID), nil
}

// build a ReactionMessage (emoji=="" = remove, else add)
func buildReactionMessage(chatJID types.JID, targetMsgID types.MessageID, emoji string) *waE2E.Message {
	return &waE2E.Message{
		ReactionMessage: &waE2E.ReactionMessage{
			Key: &waE2E.MessageKey{
				FromMe:    proto.Bool(true),
				ID:        proto.String(string(targetMsgID)),
				RemoteJID: proto.String(chatJID.String()),
			},
			Text: proto.String(emoji),
		},
	}
}

// sendReaction sends a reaction with specific message ID (for RTT tracking)
func sendReaction(
	ctx context.Context,
	cli *whatsmeow.Client,
	chatJID types.JID,
	targetMsgID types.MessageID,
	reactionMsgID types.MessageID,
	emoji string,
) error {

	rm := buildReactionMessage(chatJID, targetMsgID, emoji)

	_, err := cli.SendMessage(ctx, chatJID, rm, whatsmeow.SendRequestExtra{
		ID: reactionMsgID,
	})
	if err != nil {
		return fmt.Errorf("SendMessage (reaction) failed: %w", err)
	}

	fmt.Printf("[REACT] emoji=%q targetMsg=%s reactionMsgID=%s\n",
		emoji, targetMsgID, reactionMsgID,
	)
	return nil
}

// ---- main ----

func main() {
	flag.Parse()

	if *toJID == "" {
		fmt.Println("ERROR: -to is required, e.g. -to 919389442656@s.whatsapp.net")
		os.Exit(1)
	}

	ctx := context.Background()

	log := waLog.Stdout("Main", "DEBUG", true)
	dbLog := waLog.Stdout("Database", "DEBUG", true)

	// init probe logging DB
	pdb, err := initProbeDB(*probeDBPath)
	if err != nil {
		log.Errorf("Failed to open probe DB: %v", err)
		return
	}
	defer pdb.Close()
	log.Infof("Probe DB at %s", *probeDBPath)

	// WhatsMeow store
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on", *dbFile)
	log.Infof("Using WhatsMeow store: %s", dsn)

	storeContainer, err := sqlstore.New(ctx, "sqlite3", dsn, dbLog)
	if err != nil {
		log.Errorf("Failed to connect to WhatsMeow DB: %v", err)
		return
	}

	device, err := storeContainer.GetFirstDevice(ctx)
	if err != nil {
		log.Errorf("Failed to get device: %v", err)
		return
	}

	cli := whatsmeow.NewClient(device, waLog.Stdout("Client", "DEBUG", true))

	tracker := NewProbeTracker(pdb, log)
	cli.AddEventHandler(tracker.Handler)

	// Login / QR pairing
	if cli.Store.ID == nil {
		log.Infof("No stored ID, starting new login with QR")
		qrChan, err := cli.GetQRChannel(ctx)
		if err != nil {
			log.Errorf("Failed to get QR channel: %v", err)
			return
		}

		err = cli.Connect()
		if err != nil {
			log.Errorf("Failed to connect: %v", err)
			return
		}

		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("Scan this QR in WhatsApp → Linked devices:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else {
				log.Infof("Login event: %s", evt.Event)
			}
		}
	} else {
		log.Infof("Device already logged in as %s, connecting", cli.Store.ID.String())
		if err := cli.Connect(); err != nil {
			log.Errorf("Failed to connect: %v", err)
			return
		}
	}

	jid, err := types.ParseJID(*toJID)
	if err != nil {
		log.Errorf("Invalid -to JID: %v", err)
		return
	}
	log.Infof("Target JID: %s", jid.String())

	// small settle delay
	time.Sleep(2 * time.Second)

	// Send base message and get its ID
	baseMsgID, err := sendHello(ctx, cli, jid)
	if err != nil {
		log.Errorf("Failed to send Hello World: %v", err)
		return
	}
	log.Infof("Base message ID for reactions: %s", baseMsgID)

	interval := time.Duration(*rateMS) * time.Millisecond
	log.Infof("Starting probe loop: every %v (REMOVE then ADD %s)", interval, *emoji)

	// probe loop: REMOVE → ADD every cycle, both logged in probe DB
	go func() {
		probeID := 0
		for {
			probeID++

			// 1) REMOVE reaction
			removeID := cli.GenerateMessageID()
			sendTSremove := time.Now().UnixNano()
			if err := sendReaction(ctx, cli, jid, baseMsgID, removeID, ""); err != nil {
				log.Errorf("Probe %d REMOVE failed: %v", probeID, err)
			} else {
				if err := pdb.recordSend(probeID, "remove", string(baseMsgID), string(removeID), sendTSremove, ""); err != nil {
					log.Errorf("recordSend REMOVE failed: %v", err)
				}
			}

			time.Sleep(500 * time.Millisecond)

			// 2) ADD reaction
			addID := cli.GenerateMessageID()
			sendTSadd := time.Now().UnixNano()
			if err := sendReaction(ctx, cli, jid, baseMsgID, addID, *emoji); err != nil {
				log.Errorf("Probe %d ADD failed: %v", probeID, err)
			} else {
				if err := pdb.recordSend(probeID, "add", string(baseMsgID), string(addID), sendTSadd, ""); err != nil {
					log.Errorf("recordSend ADD failed: %v", err)
				}
			}

			time.Sleep(interval)
		}
	}()

	log.Infof("Client connected. Probing. Press Ctrl+C to stop.")

	// Wait for Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	log.Infof("Disconnecting...")
	cli.Disconnect()
	time.Sleep(1 * time.Second)
	log.Infof("Bye.")
}
