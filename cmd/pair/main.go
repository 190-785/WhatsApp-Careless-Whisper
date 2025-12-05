package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func main() {
	dbPath := flag.String("db", "client.db", "Whatsmeow client database (SQLite file)")
	flag.Parse()

	ctx := context.Background()

	// Setup database
	dbLog := waLog.Stdout("Database", "INFO", true)
	dbDSN := fmt.Sprintf("file:%s?_foreign_keys=on", *dbPath)
	container, err := sqlstore.New(ctx, "sqlite3", dbDSN, dbLog)
	if err != nil {
		panic(fmt.Errorf("failed to open database: %w", err))
	}

	// Get or create device
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		panic(fmt.Errorf("failed to get device: %w", err))
	}

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	// Event handler for QR code and pairing
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.QR:
			fmt.Println("QR code received. Scan with WhatsApp mobile app:")
			qrterminal.GenerateHalfBlock(v.Codes[0], qrterminal.L, os.Stdout)
			fmt.Println("\nScanning instructions:")
			fmt.Println("1. Open WhatsApp on your phone")
			fmt.Println("2. Go to Settings > Linked Devices")
			fmt.Println("3. Tap 'Link a Device'")
			fmt.Println("4. Scan the QR code above")

		case *events.PairSuccess:
			fmt.Printf("\n✓ Successfully paired! Device: %s\n", v.ID.String())
			fmt.Println("You can now run the probe program.")

		case *events.Connected:
			fmt.Println("✓ Connected to WhatsApp servers")
			fmt.Println("Pairing complete. Press Ctrl+C to exit.")
		}
	})

	// Connect
	if err := client.Connect(); err != nil {
		panic(fmt.Errorf("failed to connect: %w", err))
	}

	// Wait for interrupt signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("\nDisconnecting...")
	client.Disconnect()
}
