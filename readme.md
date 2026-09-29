# stackchan-server

Relay between M5Stack Stack-chan robots and web browsers for **Embody Mode**. It lets you control the robot and see its sensors from any browser.

- **Robot:** opens an outbound WebSocket to the server and registers. It then shows a QR code for pairing, streams telemetry, and receives commands.
- **Browser:** scans the robot's QR code to pair, then gets a live dashboard with command buttons. No app to install.
- **Server:** one Go binary with the web page built in, and all state in memory.

Workspace notes, including the firmware side and the design, live in `~/work-stai/stackchan-mj/AGENTS.md`.

## Run

```bash
go run ./cmd/stackchan-server              # listens on :8765
go run ./cmd/fake-robot                    # simulated robot, in a second terminal
```

- The first run generates the robot token at `~/.config/stackchan-server/robot-token`.
- The fake robot prints a pairing URL. Open it in a browser (or phone) on the same network.
- `-public-url` sets the base URL put into QR codes. The default is `http://<LAN IP>:<port>`.
- Open the dashboard with the same host as the QR code (e.g. `http://192.168.0.18:8765/`, not `localhost`). The pairing cookie belongs to that host.
- Run `go test -race ./...` for the tests.

## Protocol

The frame envelope and handshake are the same as yolovm-pilot's (`~/work-stai/stai-yolovm/docs/yolovm-pilot/wire-protocol.md`), so the same robot can also register there as a `robot`-class worker.

**Connect:** `ws://<server>/api/workers/connect` with these headers:

- `Authorization: Bearer <robot token>`
- `X-Yolovm-Worker-Id: <robot id>`: 1–64 of `[A-Za-z0-9._-]`

**Frames:** every WebSocket text message is one JSON object: `{"kind": "...", "meta": {...}, "body": {...}}`.

| Direction | Kind | Body |
|---|---|---|
| robot → server | `Register` (must be first) | `{"class": "robot", "capabilities": {"model", "firmware", "commands": ["nod"], "measurements": [...]}}` |
| robot → server | `RobotTelemetry` | `{"measurements": {"battery_pct": 87.5}}` (values are numbers) |
| robot → server | `Heartbeat` | `{}`, every 30 s |
| server → robot | `Accepted` / `Rejected` | `{}` / `{"reason": "..."}` |
| server → robot | `PairCode` | `{"code", "url", "expires_in_s"}`: show `url` as a QR code |
| server → robot | `Paired` | `{"viewers": 1}` |
| server → robot | `RobotCommand` | `{"command": "nod", "args": {}}` |

**Liveness:** the server sends a WebSocket ping every 5 s and drops the robot after 60 s without traffic. Reconnect with backoff from 1 s up to 30 s.

**Pairing:** codes are one-time and valid for `-pair-ttl` (default 5 min). A used or expiring code is replaced by a fresh `PairCode`, so the QR on the robot's screen always works.

## Status

This is the first vertical slice: it runs on the LAN over plain `ws://` with one shared robot token. Next steps:

1. TLS (`wss://`)
2. Per-device identity: ESP32-S3 DS peripheral, see yolovm phase 4.5
3. Persisting pairings
4. User accounts
