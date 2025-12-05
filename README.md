# WhatsApp Careless Whisper - RTT Probe Tool

This project is a specialized tool designed to measure the Round Trip Time (RTT) of WhatsApp messages by sending and monitoring reaction updates. It is built to replicate the results and methodology described in the research paper **"Careless Whisper: Stealthy User Tracking via WhatsApp RTT Analysis"**.

**⚠️ DISCLAIMER: This software is for EDUCATIONAL and RESEARCH purposes only. Do not use this tool for stalking, harassment, or any unauthorized surveillance. The authors and contributors are not responsible for any misuse of this software.**

## 📚 Inspiration & Credits

*   **Research Paper**: This project implements the probing mechanism described in [Careless Whisper: Stealthy User Tracking via WhatsApp RTT Analysis](https://arxiv.org/pdf/2411.11194).
*   **Original Library**: This repository is a fork of [whatsmeow](https://github.com/tulir/whatsmeow), a robust Go library for the WhatsApp web multidevice API.
*   **Contribution**: The core library remains largely unchanged. The custom probing logic and pairing tools were added in the `cmd/probe` and `cmd/pair` directories.

## 🚀 Features

*   **Automated Probing**: Continuously sends reaction updates to a specific message.
*   **Alternating Actions**: Toggles between adding (`ADD`) and removing (`REMOVE`) reactions to measure RTT for different protocol interactions.
*   **Precision Timing**: Records send timestamps and receipt timestamps with nanosecond precision.
*   **SQLite Storage**: Saves all probe data (Probe ID, Action, Message ID, Timestamps, Receipt Type) into a local SQLite database for easy analysis.
*   **Configurable**: Custom flags for target JID, probe rate, emoji, base message, and maximum runtime.

## 🛠️ Installation & Usage

### Prerequisites
*   [Go](https://go.dev/dl/) (1.20 or later)
*   A WhatsApp account (linked via QR code)

### 1. Build the Tools
Clone the repository and build the pairing and probing binaries:

```bash
# Build the pairing tool
go build -o pair ./cmd/pair

# Build the probe tool
go build -o probe ./cmd/probe
```

### 2. Authenticate (Pairing)
Run the pairing tool to link your WhatsApp account. This creates a `client.db` file containing your session keys.

```bash
./pair -db client.db
```
*   Scan the QR code that appears in your terminal using your WhatsApp mobile app (Settings > Linked Devices > Link a Device).
*   Once paired, you can exit the tool.

### 3. Run the Probe
Start the probe by specifying the target user and parameters.

```bash
./probe \
  -db client.db \
  -probe-db probes.db \
  -to 1234567890@s.whatsapp.net \
  -base-msg-id <EXISTING_MESSAGE_ID> \
  -rate 1000 \
  -emoji "🙂" \
  -max-runtime 6h
```

**Flags:**
*   `-db`: Path to the whatsmeow session database (default: `client.db`).
*   `-probe-db`: Path to the output database for measurements (default: `probes.db`).
*   `-to`: The target JID (e.g., `919xxxxxxxxx@s.whatsapp.net`).
*   `-base-msg-id`: (Optional) ID of an existing message to react to. If omitted, the tool will send a new "Hello World!" message to use as a base.
*   `-rate`: Interval between probes in milliseconds (default: `3000`).
*   `-emoji`: The emoji to use for reactions (default: `🙂`).
*   `-max-runtime`: Duration to run before stopping (e.g., `6h`, `30m`).

### 4. Analyze Data
The results are stored in `probes.db`. You can inspect the data using any SQLite viewer or query it directly:

```sql
SELECT 
    probe_id, 
    action, 
    receipt_type, 
    (receipt_ts_ns - send_ts_ns) / 1000000.0 AS rtt_ms 
FROM probes;
```

## 📈 Replication Results

I am currently running the replication experiments. The detailed results, including RTT analysis and findings compared to the original paper, will be updated here soon.

## 📂 Project Structure

*   `cmd/pair/`: Contains the code for the authentication/pairing tool.
*   `cmd/probe/`: Contains the main probing logic, RTT measurement, and database recording.
*   `client.db`: (Generated) Stores WhatsApp session data.
*   `probes.db`: (Generated) Stores the collected RTT measurement data.

---
*This project is a replication study based on the "Careless Whisper" paper.*
