# stackchan-server

Relay between M5Stack Stack-chan robots and web browsers for **Embody Mode**. It lets you see through the robot and control it from any browser, with no app to install.

- **Robot:** opens an outbound WebSocket to the server and registers. It then shows a QR code for pairing, streams telemetry and events, and runs commands.
- **Browser:** scans the robot's QR code to pair, then gets a live dashboard.
- **Server:** one Go binary with the web page built in. State lives in memory and is saved to a JSON file, so pairings survive a restart.

The robot side is the Embody Mode app in the StackChan firmware fork (`firmware/main/apps/app_embody_mode/`, branch `mj-remote`). Workspace notes and the trust design live in `~/work-stai/stackchan-mj` (`AGENTS.md`, `docs/design.md`).

## Dashboard

For each paired robot:

- **Status:** battery, charging, head yaw/pitch, Wi-Fi, free memory, uptime, brightness, volume.
- **Latency:** "Ping ×10" splits the round trip into browser ↔ server, server ↔ robot, and the robot's app loop.
- **Face:** six emotions, `say` (speech bubble).
- **Head:** nod, shake, home, and yaw/pitch sliders with ±5/±15 chips. A slider holds your target and shows the robot's reported angle ("now …"). Pitch is limited to 5–85° (M5Stack's safe range).
- **LEDs:** one colour for all 12 LEDs, "Random" (a random colour per LED, for a quick test) and "Off"; effects drawn by the robot (Rainbow, Breathe, Chase, Blink) in that colour at three speeds; and a picker per LED.
- **Settings:** brightness and volume.
- **Screen:** stickers over the face (heart, angry, sweat, shy, dizzy), 12 emoji (smile, grin, laugh, wink, love, cool, surprised, thinking, sleepy, cry, sob, angry) sent as a full-screen picture, or a picture from the phone. The picture is scaled to 320x240 in the browser and replaces the face until "Face".
- **Camera & mic:** live video (JPEG, about 5 fps) and the robot's microphone, played through Web Audio. The robot streams only while someone watches or listens, and shows a red LIVE badge meanwhile.
- **Speaker:** "Talk" (tap to start, tap again to stop) streams your microphone to the robot, "Play sound file" plays any audio file the browser can decode, and "Beep" is a test tone. Audio is resampled to 24 kHz in the browser and the robot's mouth moves while it plays. Browsers give the microphone only to secure pages: use `http://localhost` on the server machine, or run the server with `-tls-listen :8766` and open `https://<LAN IP>:8766` on the phone (the plain page links to it; accept the self-signed certificate warning once). Files and Beep work anywhere.
- **Infrared:** point a TV remote at the robot and press a button: the last code shows up with "Send again" and "Save…" (a named button, kept in this browser). "NEC address / command" sends a code by hand. "Test LED (3 s)" lights the IR LED long enough to see it through a phone camera; "Self-test" checks that the robot hears its own signal (carrier and timing).
- **Power LED** (under LEDs): on, blink, fast, off, or "Charging" (the charger drives it).
- **NFC:** "Reader on" / "Reader off". While on (the default), holding a tag or card near the robot adds an `nfc tag` event with its UID and type. For NTAG stickers it also shows the first NDEF record (a URL or text). Taking it away adds `nfc removed`.
- **Sensors:** a table fed by the robot's telemetry: acceleration per axis (g) and rotation rate, servo load and temperature, servo supply voltage, chip temperature, room light (lux) and proximity.
- **Auto brightness** (under Settings, for robots with a light sensor): the robot sets its backlight from the room light. Moving the brightness slider switches it off.
- **Proximity sensor** (under Settings): on/off. Its IR LED next to the camera blinks about 10 times a second; switch it off, e.g., while using infrared.
- **Events:** shake, head touch (press with the touched zones, release with its duration, swipe forward / back), screen taps with coordinates, NFC tags (UID, type, and the link or text stored on the tag), and screensaver on/off. The list sits right under Status. The server replays each robot's last 20 events when a browser connects, so a phone that was asleep still sees them.
- **Screen & power** (right after Events): "Screensaver on" / "Screensaver off", and "Standby" for 1–60 min.
  - **Status** shows the screensaver as `off`, `auto` or `manual`.
  - **Auto:** the robot blanks after 60 s (firmware option) without touch **and** without commands or live media, so using it remotely keeps it awake.
  - **Manual:** a **double tap** on the robot or "Screensaver on". The robot keeps it until a touch or "Screensaver off".
  - **Commands wake the screen** (checkbox, on by default): the dashboard follows every command (except ping), and every picture, with `screensaver off` when the screen is blank. The robot stays explicit; the browser decides.
  - **Standby:** the robot tells the server, then goes offline with backlight, LEDs, camera and mic off. It comes back after the time or on a touch. Meanwhile the card shows "standby until HH:MM".

Controls appear only for the commands a robot lists in its capabilities.

## Run

```bash
go run ./cmd/stackchan-server              # listens on :8765
go run ./cmd/fake-robot                    # simulated robot, in a second terminal
```

- The first run generates the robot token at `~/.config/stackchan-server/robot-token`.
- `fake-robot` implements the whole command set: a test-pattern camera, a 440 Hz "microphone", picture checks, and random events. It prints a pairing URL; open it in a browser or phone on the same network.
- `-public-url` sets the base URL put into QR codes. The default is `http://<LAN IP>:<port>`.
- `-state-file` (default `~/.local/state/stackchan-server/state.json`, `""` disables) keeps pairings and known robots across restarts: browsers stay paired, and robots show up offline with their last telemetry and events until they reconnect. The whole file is rewritten every 5 s when something changed, right after a pairing, and on shutdown. It holds session IDs, so it is mode 0600. It is a stopgap until a real database.
- `-tls-listen :8766` also serves the dashboard over HTTPS, for the phone's microphone ("Talk"). Without `-tls-cert`/`-tls-key` it creates a self-signed certificate for localhost, this host name and every local IP, and keeps it in `~/.config/stackchan-server/` so a phone accepts it only once. Robots stay on `-listen`. The pairing cookie is shared by both ports (same host).
- `-ui-dir internal/server/ui` serves the dashboard from disk on every request (development). UI edits then need only a page reload.
- Open the dashboard with the same host as the QR code (e.g. `http://192.168.1.10:8765/`, not `localhost`). The pairing cookie belongs to that host.
- Run `go test -race ./...` for the tests.

## Deploy

v0.1.0 (relay, pairing, telemetry, nod) runs at **https://chan.w42.eu**. Newer features are tested on the LAN first and are not released yet.

1. **Release:** push a `v*` tag. `.github/workflows/release.yml` builds and pushes `ghcr.io/mj41/stackchan-server:<tag>`.
2. **Deploy:** run the new image where you host it, pinned by digest.
3. **Robot token:** keep it in your host's secret store, never in git.

Constraints:

- **One replica:** state is in memory, so the Deployment uses one replica with `Recreate`. The image disables the state file (`-state-file ""`) because the root file system is read-only; a pod restart forgets pairings until a volume is mounted for it.
- **No request timeout** on the proxy in front, so it never cuts the robot WebSocket, the browser SSE, or the media sockets.
- **TLS:** it ends at the gateway, so `-public-url https://…` also makes the session cookie `Secure`.
- **Audio bandwidth:** microphone audio is raw PCM (about 48 KB/s). Compress it before offering audio through the cloud.

## Protocol

The frame envelope and handshake are the same as yolovm-pilot's (`~/work-stai/stai-yolovm/docs/yolovm-pilot/wire-protocol.md`), so the same robot can also register there as a `robot`-class worker.

**Connect:** `ws://<server>/api/workers/connect` with these headers:

- `Authorization: Bearer <robot token>`
- `X-Yolovm-Worker-Id: <robot id>`: 1–64 of `[A-Za-z0-9._-]`

### JSON frames

Every WebSocket text message is one JSON object: `{"kind": "...", "meta": {...}, "body": {...}}`.

| Direction | Kind | Body |
|---|---|---|
| robot → server | `Register` (must be first) | `{"class": "robot", "capabilities": {"model", "firmware", "commands": [...], "measurements": [...]}}` |
| robot → server | `RobotTelemetry` | `{"measurements": {"battery_pct": 87.5}}` (values are numbers) |
| robot → server | `RobotEvent` | `{"name", "data"}`: `shake`, `head_press` with zone intensities `{"z0", "z1", "z2"}` (0–3), `head_release` with `{"ms"}`, `head_swipe_forward`, `head_swipe_backward`, `screen_tap` with `{"x", "y"}`, `screensaver_on` with `{"manual": 0 or 1}`, `screensaver_off`, `standby` with `{"minutes"}`, `standby_end` with `{"touched"}`, `nfc_tag` with `{"uid": "04:A2:…", "type", "atqa", "sak"}` plus `"text"` when the tag holds an NDEF URI or text record, `nfc_removed` with `{"uid"}`, `proximity_near` / `proximity_far` with `{"value"}`, `ir_received` with `{"protocol": "nec" or "raw", "address", "command", "raw"}` (raw marks/spaces in µs, always present). Values are numbers or strings. Browsers also get a per-robot `seq` |
| robot → server | `RobotPong` | `{"id", "queue_ms"}`: answer to `ping`; `queue_ms` is time spent waiting on the robot |
| robot → server | `Heartbeat` | `{}`, every 30 s |
| server → robot | `Accepted` / `Rejected` | `{}` / `{"reason": "..."}` |
| server → robot | `PairCode` | `{"code", "url", "expires_in_s"}`: show `url` as a QR code |
| server → robot | `Paired` | `{"viewers": 1}` |
| server → robot | `RobotCommand` | `{"command": "nod", "args": {}}` |

### Commands

`RobotCommand.command`, with `args`:

| Command | Args |
|---|---|
| `ping` | `{"id"}`: the robot answers with `RobotPong`. The server times its leg and sends a `pong` SSE event only to the browser that pinged. |
| `nod`, `shake`, `home` | none |
| `look` | `{"yaw": -128..128, "pitch": 5..85}` in degrees |
| `screensaver` | `{"on": bool}`: blank the screen (manual) or wake it |
| `standby` | `{"minutes": 1..120}`: the robot sends a `standby` event, goes offline, and reconnects after the time or on a touch (then sends `standby_end`). `/api/robots` shows `standby_until` while it is away |
| `emotion` | `{"name": "neutral\|happy\|angry\|sad\|doubt\|sleepy"}` |
| `say` | `{"text", "seconds"}`: speech bubble |
| `sticker` | `{"name": "heart\|angry\|sweat\|shy\|dizzy", "seconds"}`: decoration over the face |
| `face` | none: back to the face after a picture |
| `image` | capability only: pictures arrive as binary `0x10` |
| `leds` | `{"left": "#rrggbb", "right": "#rrggbb"}` fades a whole side. `{"pixels": [...]}` sets up to 12 single LEDs (left 0–5, right 6–11; `null` skips one). `{"effect": "rainbow\|breathe\|chase\|blink\|off", "color", "speed": 0.2..5, "seconds"}` runs an animation on the robot (`seconds` 0 = until the next `leds`) |
| `brightness` | `{"value": 1..100}` sets it by hand and ends auto-brightness; `{"auto": bool}` switches brightness that follows the room light (robots with a light sensor report `auto_brightness`) |
| `volume` | `{"value": 0..100}` |
| `camera`, `mic` | `{"on": bool}`: sent by the server, not by browsers, while someone watches or listens |
| `ir_send` | `{"address", "command"}` sends an NEC code; `{"raw": "9000,4500,562,…", "carrier_hz": 38000}` sends marks and spaces in µs (e.g. what `ir_received` reported). `"loopback": true` lets the robot hear its own signal (self-test) |
| `proximity` | `{"on": bool}`: the proximity sensor, whose IR LED next to the camera pulses ~10×/s. Off stops the LED and the approach events; light and auto-brightness keep working. Telemetry `proximity_on` |
| `power_led` | `{"mode": "on\|off\|blink\|fast\|charging"}`: the red power LED; `charging` hands it back to the charger |
| `nfc` | `{"on": bool}`: NFC tag polling, on by default. Listed only when the robot found its reader |

### Binary messages

The first byte is the type, followed by the payload.

| Type | Direction | Payload |
|---|---|---|
| `0x01` | robot → server → browser | camera frame, JPEG |
| `0x02` | robot → server → browser | microphone: sample rate (uint16 LE), then s16le mono PCM |
| `0x03` | browser → server → robot | speaker: sample rate (uint16 LE), then s16le mono PCM. Sent on the media socket; forwarded only to robots that list `speaker` |
| `0x10` | server → robot | picture, JPEG 320x240, shown instead of the face |

### Browser API

All endpoints need the session cookie of a browser that paired with the robot.

| Endpoint | Purpose |
|---|---|
| `GET /pair?code=…` | QR target; adds the robot to this browser's session |
| `GET /api/robots` | paired robots (JSON) |
| `GET /api/events` | SSE: `robot` (full state; telemetry `screensaver` 0 off / 1 auto / 2 manual), `robot_event` (to every paired browser; the last 20 are replayed on connect), `pong` (only to the browser that pinged) |
| `POST /api/robots/{id}/command` | `{"command", "args"}` as JSON |
| `POST /api/robots/{id}/picture` | `image/jpeg` body, up to 192 KB |
| `GET /api/robots/{id}/media?video=1&audio=1` | WebSocket, same origin only, carrying binary `0x01`/`0x02` |

**Liveness:** the server sends a WebSocket ping every 5 s and drops the robot after 60 s without traffic. Reconnect with backoff from 1 s up to 30 s.

**Pairing:** codes are one-time and valid for `-pair-ttl` (default 5 min). A used or expiring code is replaced by a fresh `PairCode`, so the QR on the robot's screen always works.

## Status

This is an early prototype, tested on real hardware on the LAN.
- **Auth:** one shared robot token per deployment.
- **State:** in memory, plus a JSON snapshot (`-state-file`) for restarts. Pairing codes and live connections are not saved.
- **Plan:** the next steps follow `stackchan-mj/docs/design.md`:
  1. owner keys and signed config/grants
  2. rendezvous at `chan.w42.eu`
  3. our public apps on `appchan.w42.eu`
  4. private apps on the owner's LAN server
  5. per-device identity with the ESP32-S3 DS peripheral

## Credits

The dashboard's emoji are [Fluent Emoji](https://github.com/microsoft/fluentui-emoji) (Flat style) by Microsoft, MIT license; see [internal/server/ui/emoji/LICENSE](internal/server/ui/emoji/LICENSE).
