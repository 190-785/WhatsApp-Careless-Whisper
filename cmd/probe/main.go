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
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type probeConfig struct {
	clientDBPath string
	probeDBPath  string
	toJIDStr     string
	msgID        string
	rate         time.Duration
	emoji        string
	maxRuntime   time.Duration
}

// create or migrate probes table
func initProbeDB(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS probes (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    probe_id       INTEGER NOT NULL,
    msg_id         TEXT    NOT NULL,
    send_ts_ns     INTEGER NOT NULL,
    receipt_ts_ns  INTEGER,
    receipt_type   TEXT
);
`
	_, err := db.Exec(ddl)
	return err
}

func main() {
	cfg := parseFlags()

	// Logging
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})
	log.Info().Msg("Starting probe client")

	// Open probe DB
	probeDB, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=on", cfg.probeDBPath))
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to open probe DB")
	}
	defer probeDB.Close()

	if err := initProbeDB(probeDB); err != nil {
		log.Fatal().Err(err).Msg("Failed to init probe DB schema")
	}

	// WhatsApp client store
	container, err := sqlstore.New(
		"sqlite3",
		fmt.Sprintf("file:%s?_foreign_keys=on", cfg.clientDBPath),
		waLog.Stdout("DB", "INFO", true),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create WhatsApp store container")
	}
	defer container.Close()

	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to get device store")
	}
	if deviceStore == nil {
		log.Fatal().Msg("No device found in client DB. Pair a device first using the example-login tool.")
	}

	clientLog := waLog.Stdout("Client", "DEBUG", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	// Event handler: record receipts for reaction messages
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Receipt:
			handleReceiptEvent(probeDB, v)
		}
	})

	if client.Store.ID == nil {
		log.Fatal().Msg("No logged in session in client DB. Pair first before running probe.")
	}

	// Connect
	if err := client.Connect(); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to WhatsApp")
	}
	defer client.Disconnect()

	// Global context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parse target JID
	toJID, err := types.ParseJID(cfg.toJIDStr)
	if err != nil {
		log.Fatal().Err(err).Str("jid", cfg.toJIDStr).Msg("Invalid target JID")
	}

	// Determine base message ID: either provided or send "Hello World!"
	baseMsgID := cfg.msgID
	if baseMsgID == "" {
		log.Info().Str("to", toJID.String()).Msg("No -msg-id provided, sending base message 'Hello World!'")
		baseMsgID, err = sendBaseMessage(ctx, client, toJID)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to send base message")
		}
		log.Info().Str("msg_id", baseMsgID).Msg("Base message ID for reactions")
	} else {
		log.Info().Str("msg_id", baseMsgID).Msg("Using provided base message ID for reactions")
	}

	// Probe loop (reactions only)
	start := time.Now()
	log.Info().
		Dur("rate", cfg.rate).
		Dur("max_runtime", cfg.maxRuntime).
		Msg("Starting reaction probe loop")

	go runProbeLoop(ctx, client, probeDB, cfg, toJID, baseMsgID, start, cancel)

	// Wait for signal or context cancel
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info().Str("signal", sig.String()).Msg("Signal received, shutting down")
		cancel()
	case <-ctx.Done():
		log.Info().Msg("Context canceled (likely max runtime reached), shutting down")
	}

	// Allow some time for pending DB writes
	time.Sleep(1 * time.Second)
	log.Info().Msg("Probe stopped cleanly")
}

// parseFlags reads CLI flags and returns config
func parseFlags() probeConfig {
	var (
		clientDB  = flag.String("db", "", "Path to WhatsApp client SQLite DB (login session)")
		probeDB   = flag.String("probe-db", "", "Path to probe SQLite DB (measurement data)")
		to        = flag.String("to", "", "Target JID, e.g. 919389442656@s.whatsapp.net")
		msgID     = flag.String("msg-id", "", "Existing base message ID to react to (optional)")
		rateMS    = flag.Int("rate", 3000, "Probe interval in milliseconds")
		emoji     = flag.String("emoji", "🙂", "Emoji to use for reactions")
		maxRunSec = flag.Int("max-runtime", 0, "Max runtime in seconds (0 = unlimited)")
	)

	flag.Parse()

	if *clientDB == "" || *probeDB == "" || *to == "" {
		fmt.Fprintf(os.Stderr, "Usage: probe -db client.db -probe-db probes.db -to <jid> [-msg-id <id>] [-rate 3000] [-emoji 🙂] [-max-runtime 21600]\n")
		os.Exit(1)
	}

	cfg := probeConfig{
		clientDBPath: *clientDB,
		probeDBPath:  *probeDB,
		toJIDStr:     *to,
		msgID:        *msgID,
		rate:         time.Duration(*rateMS) * time.Millisecond,
		emoji:        *emoji,
		maxRuntime:   time.Duration(*maxRunSec) * time.Second,
	}

	return cfg
}

// sendBaseMessage sends "Hello World!" and returns its message ID
func sendBaseMessage(ctx context.Context, client *whatsmeow.Client, toJID types.JID) (string, error) {
	msg := &waProto.Message{
		Conversation: proto.String("Hello World!"),
	}
	resp, err := client.SendMessage(ctx, toJID, msg)
	if err != nil {
		return "", err
	}
	log.Info().
		Str("msgID", resp.ID).
		Str("to", toJID.String()).
		Msg("[SEND] Hello World!")

	return resp.ID, nil
}

// runProbeLoop sends REMOVE + ADD reactions at the chosen interval
func runProbeLoop(
	ctx context.Context,
	client *whatsmeow.Client,
	probeDB *sql.DB,
	cfg probeConfig,
	toJID types.JID,
	baseMsgID string,
	start time.Time,
	cancel context.CancelFunc,
) {
	ticker := time.NewTicker(cfg.rate)
	defer ticker.Stop()

	probeID := 1

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// Stop if max runtime exceeded
			if cfg.maxRuntime > 0 && time.Since(start) >= cfg.maxRuntime {
				log.Info().
					Dur("max_runtime", cfg.maxRuntime).
					Msg("Max runtime reached, stopping probe loop")
				cancel()
				return
			}

			// 1) REMOVE reaction
			removeSendTS := time.Now().UnixNano()
			rmID, err := sendReaction(ctx, client, toJID, baseMsgID, "", types.ReactionRemove)
			if err != nil {
				log.Error().Err(err).Int("probe_id", probeID).Msg("Failed to send REMOVE reaction")
			} else {
				if err := insertProbe(probeDB, probeID, rmID, removeSendTS, "remove_sent"); err != nil {
					log.Error().Err(err).Str("msg_id", rmID).Msg("Failed to insert REMOVE probe row")
				}
			}

			// Short delay between remove and add to decorrelate slightly
			time.Sleep(50 * time.Millisecond)

			// 2) ADD reaction
			addSendTS := time.Now().UnixNano()
			addID, err := sendReaction(ctx, client, toJID, baseMsgID, cfg.emoji, types.ReactionAdd)
			if err != nil {
				log.Error().Err(err).Int("probe_id", probeID).Msg("Failed to send ADD reaction")
			} else {
				if err := insertProbe(probeDB, probeID, addID, addSendTS, "add_sent"); err != nil {
					log.Error().Err(err).Str("msg_id", addID).Msg("Failed to insert ADD probe row")
				}
			}

			log.Debug().
				Int("probe_id", probeID).
				Time("tick_time", now).
				Msg("Probe tick completed (REMOVE + ADD)")
			probeID++
		}
	}
}

// sendReaction wraps whatsmeow's SendReaction call and returns the reaction message ID
func sendReaction(
	ctx context.Context,
	client *whatsmeow.Client,
	chat types.JID,
	baseMsgID string,
	emoji string,
	rType types.ReactionType,
) (string, error) {
	msgKey := &waProto.MessageKey{
		ID:        baseMsgID,
		FromMe:    true,
		RemoteJID: proto.String(chat.String()),
	}

	// NOTE: Depending on whatsmeow version, the signature may differ slightly.
	// This assumes:
	//   SendReaction(ctx, chat, msgKey, emoji, rType) (whatsmeow.SendResponse, error)
	resp, err := client.SendReaction(ctx, chat, msgKey, emoji, rType)
	if err != nil {
		return "", err
	}

	log.Debug().
		Str("targetMsg", baseMsgID).
		Str("reactionMsgID", resp.ID).
		Str("emoji", emoji).
		Str("type", string(rType)).
		Msg("[REACT] sent")

	return resp.ID, nil
}

// insertProbe writes a new probe row (one per reaction message sent)
func insertProbe(db *sql.DB, probeID int, msgID string, sendTS int64, kind string) error {
	_, err := db.Exec(
		`INSERT INTO probes (probe_id, msg_id, send_ts_ns, receipt_ts_ns, receipt_type)
         VALUES (?, ?, ?, NULL, ?)`,
		probeID, msgID, sendTS, kind,
	)
	return err
}

// handleReceiptEvent updates existing probe rows when receipts arrive
func handleReceiptEvent(probeDB *sql.DB, v *events.Receipt) {
	if v.Type != types.ReceiptTypeSender && v.Type != types.ReceiptTypeInactive {
		return
	}

	receiptTS := time.Now().UnixNano()
	typ := string(v.Type)

	for _, id := range v.MessageIDs {
		res, err := probeDB.Exec(
			`UPDATE probes
             SET receipt_ts_ns = ?, receipt_type = ?
             WHERE msg_id = ? AND receipt_ts_ns IS NULL`,
			receiptTS, typ, id,
		)
		if err != nil {
			log.Error().Err(err).Str("msg_id", id).Msg("recordReceipt update failed")
			continue
		}
		aff, _ := res.RowsAffected()
		if aff == 0 {
			// Not fatal, just noisy: we might get receipts for messages we did not mark as probes
			log.Debug().Str("msg_id", id).Msg("recordReceipt: no matching probe row")
		} else {
			log.Debug().
				Str("msg_id", id).
				Int64("rows", aff).
				Str("type", typ).
				Msg("recordReceipt: updated probe row")
		}
	}
}
