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

## Deploy

A public instance runs at **https://chan.w42.eu**.

1. **Release:** push a `v*` tag. `.github/workflows/release.yml` builds and pushes `ghcr.io/mj41/stackchan-server:<tag>`.
2. **Deploy:** run the new image where you host it, pinned by digest.
3. **Robot token:** keep it in your host's secret store, never in git.

Constraints:

- **One replica:** all state is in memory, so the Deployment uses one replica with `Recreate`.
- **No request timeout** on the proxy in front, so it never cuts the robot WebSocket or browser SSE.
- **TLS:** it ends at the gateway, so `-public-url https://…` also makes the session cookie `Secure`.

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

This is the first vertical slice. It runs on the LAN (`ws://`) for development, and in the cloud at `wss://chan.w42.eu` with one shared robot token per deployment. Next steps:

1. Per-device identity: ESP32-S3 DS peripheral, see yolovm phase 4.5
2. Persisting pairings across restarts
3. User accounts
