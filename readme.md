# stackchan-server

Relay between M5Stack Stackchan robots and web browsers for **Embody Mode**. It lets you see through the robot and control it from any browser, with no app to install.

- **Robot:** opens an outbound WebSocket to the server and registers. It then shows a QR code for pairing, streams telemetry and events, and runs commands.
- **Browser:** scans the robot's QR code to pair, then gets a live dashboard.
- **Server:** one Go binary with the web page built in. State lives in memory and is saved to a JSON file, so pairings survive a restart.

The robot side is the [Embody Mode app](https://github.com/mj41/StackChan/tree/embody-mj41/firmware/main/apps/app_embody_mode) in the StackChan firmware fork [mj41/StackChan](https://github.com/mj41/StackChan/tree/embody-mj41) (branch `embody-mj41`). Setting up a robot with this server, from building the firmware to pairing a phone: [SETUP.md](https://github.com/mj41/StackChan/blob/embody-mj41/firmware/main/apps/app_embody_mode/SETUP.md).

Part of [home-w42-eu](https://github.com/mj41/home-w42-eu), a local first, privacy first platform for a home: this repo holds the Go implementation of its device wire protocol (the `wire` package), which the other servers use too.

> **A proof of concept, vibe coded.** Written with AI agents and tested on real hardware at
> home, but neither the code nor its security has been reviewed by humans. Use it on your
> own network, and don't trust it with anything private yet.
>
> **Want more?** Ask in the [issues](https://github.com/mj41/stackchan-server/issues), and ideally [sponsor mj41](https://github.com/sponsors/mj41) on GitHub:
> mj41 codes for attention food.

## Dashboard

For each paired robot:

- **Status:** battery, charging, head yaw/pitch, Wi-Fi, free memory, uptime, brightness, volume.
- **Latency:** "Ping ×10" splits the round trip into browser ↔ server, server ↔ robot, and the robot's app loop.
- **Face:** six emotions, `say` (speech bubble).
- **Head:** nod, shake, home, and yaw/pitch sliders with ±5/±15 chips. "Hold position" (Off by default; 30 s–5 min) keeps the servos powered at the current angle, then they go slack again. A slider holds your target and shows the robot's reported angle ("now …"). Pitch is limited to 5–85° (M5Stack's safe range).
- **LEDs:** one colour for all 12 LEDs, "Random" (a random colour per LED, for a quick test) and "Off"; effects drawn by the robot (Rainbow, Breathe, Chase, Blink) in that colour at three speeds; and a picker per LED.
- **Settings:** brightness and volume.
- **Screen:** stickers over the face (heart, angry, sweat, shy, dizzy), 12 emoji (smile, grin, laugh, wink, love, cool, surprised, thinking, sleepy, cry, sob, angry) sent as a full-screen picture, or a picture from the phone. The picture is scaled to 320x240 in the browser and replaces the face until "Face".
- **Live touch** (under Sensors): both fingers on the screen, drawn live (50 Hz) from the raw touch controller data. Events `touch_down` / `touch_up` (with duration) come from the same data.
- **Camera extras:** "Snapshot 640×480" (shown below the video, with a download link), Mirror / Flip, and raw sensor registers (read / write, answer in Events).
- **Servers:** the robot's server list (built-in, offered by servers with `-offer`, or added here with name, URL and robot token), with Switch, Pin as default / Unpin and Remove. Tokens never leave the robot. On the robot, the QR screen switches with Next and Pin makes the shown server the default (tap again to unpin; with no default the robot starts as a chooser and contacts nothing until you press Connect).
- **Head extras:** Servo power on/off, and Rotate ⟲ / Stop / ⟳ (continuous yaw, 3–30 s, only after ticking "No cable in the head's USB-C"; with USB power present the dashboard warns again).
- **Live IMU** (under Sensors): raw accelerometer, gyro and magnetometer at 100 Hz while the button is on (the robot streams only while someone watches), with "Download CSV" of up to 60 s.
- **Camera & mic:** live video (JPEG, about 5 fps) and the robot's microphone, played through Web Audio. The robot streams only while someone watches or listens, and shows a red LIVE badge meanwhile.
- **Speaker:** "Talk" (tap to start, tap again to stop) streams your microphone to the robot, "Play sound file" plays any audio file the browser can decode, and "Beep" is a test tone. Audio is resampled to 24 kHz in the browser and the robot's mouth moves while it plays. Browsers give the microphone only to secure pages: use `http://localhost` on the server machine, or run the server with `-tls-listen :8766` and open `https://<LAN IP>:8766` on the phone (the plain page links to it; accept the self-signed certificate warning once). Files and Beep work anywhere.
- **Infrared:** point a TV remote at the robot and press a button: the last code shows up with "Send again" and "Save…" (a named button, kept in this browser). "NEC address / command" sends a code by hand (a received NEC code fills it in), and "Add to list" saves it under a name, so you can try a few commands side by side. "Tap / Hold 0.5–2 s" sets how long every send holds the button (default 0.5 s: frame plus repeat codes, like a real press). "Send 3×" sends the whole code three times, for when the robot's weak IR LED only sometimes gets through. "Test LED (3 s)" lights the IR LED long enough to see it through a phone camera; "Self-test" checks that the robot hears its own signal (carrier and timing).
- **Power LED** (under LEDs): on, blink, fast, off, or "Charging" (the charger drives it).
- **NFC:** "Reader on" / "Reader off". While on (the default), holding a tag or card near the robot adds an `nfc tag` event with its UID and type. For NTAG stickers it also shows the first NDEF record (a URL or text). Taking it away adds `nfc removed`.
- **Adding a browser** (e.g. a laptop without a camera): on the "no robot paired" page, "Ask a paired phone" shows a short code; every paired browser gets a banner with the same code and **Allow** / **Deny**. Allow pairs the new browser with the approver's robots. Requests expire after 3 minutes and are rate-limited. The page can also pair with the 8-character code shown under the robot's QR code.
- **Sensors:** a table fed by the robot's telemetry: acceleration per axis (g) and rotation rate, the magnetic field per axis (µT, plus raw counts), servo load and temperature, servo supply voltage, chip temperature, room light (lux) and proximity, and power: the body battery (voltage, current, power, raw shunt voltage from the INA226) and the CoreS3 power chip (battery, USB and system voltage, charge state and phase, chip temperature, raw status registers).
- **Auto brightness** (under Settings, for robots with a light sensor): the robot sets its backlight from the room light. Moving the brightness slider switches it off.
- **Proximity sensor** (under Settings): on/off. Its IR LED next to the camera blinks about 10 times a second; switch it off, e.g., while using infrared.
- **Events:** shake, head touch (press with the touched zones, release with its duration, swipe forward / back), screen taps with coordinates, NFC tags (UID, type, and the link or text stored on the tag), and screensaver on/off. The list also shows what browsers sent (→ nod, → IR NEC 0x80 / 0x04 · hold ×4, → picture…), from every browser paired with the robot; a switch picks All, Robot only or Sent only. The list sits right under Status. The server replays each robot's last 40 events and sent commands when a browser connects, so a phone that was asleep still sees them.
- **Screen & power** (right after Events): "Screensaver on" / "Screensaver off", and "Standby" for 1–60 min.
  - **Status** shows the screensaver as `off`, `auto` or `manual`.
  - **Auto:** the robot blanks after 60 s (firmware option) without touch **and** without commands or live media, so using it remotely keeps it awake.
  - **Manual:** a **double tap** on the robot or "Screensaver on". The robot keeps it until a touch or "Screensaver off". A double tap on a button (a sprite with `tap`) is two taps instead, for games and menus.
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
- `-offer name=wss://host[,tokenfile]` (repeatable) offers robots other servers they may switch to; with a token file, the robot also gets that server's robot token. Offers are voluntary: a server decides where its robots may go, and the robot owner can also add servers by hand. Offers go only to robots with the shared token, never to invited robots (below).
- `-robot-tokens-file <file>`: other people's robots, each with its own invite token (see "Other people's robots" below). Off by default.
- `-trusted-proxies 1` behind one reverse proxy that appends the client address to `X-Forwarded-For` (Envoy, nginx with `$proxy_add_x_forwarded_for`). The default 0 ignores that header, because clients can forge it. Limits and logs use the address.
- **Limits** (always on): 20 failed robot logins per address in 10 minutes, then that address gets HTTP 429 for every login, right token included; 20 wrong pairing codes per address in 10 minutes, then 429; invited robots may send 300 messages/s and 512 KB/s on average (bursts of 1000 messages and 4 MB), and are disconnected above that. The owner's robots are not limited.
- `-ui-dir internal/server/ui` serves the dashboard from disk on every request (development). UI edits then need only a page reload.
- Open the dashboard with the same host as the QR code (e.g. `http://192.168.1.10:8765/`, not `localhost`). The pairing cookie belongs to that host.
- Run `go test -race ./...` for the tests.

### Other people's robots: invite tokens

The shared robot token is for your own robots: anyone who has it can connect a robot under
any id, and it is compiled into the firmware. For other people's robots, give each robot
its own **invite token**:

```bash
stackchan-server invite stackchan-0a1b2c3d4e50   # the robot id its screen shows (stackchan-<MAC>)
```

It prints a new token and a line. **The token** goes to the robot's owner, for their
`sdkconfig` (`CONFIG_STACKCHAN_EMBODY_TOKEN`). **The line** (`<robot id> <SHA-256 of the
token>`) goes into the file you pass as `-robot-tokens-file`; the file holds no tokens.

- A token works only for its own robot id, so an invited robot cannot pose as another one.
- The server reads the file again when it changes: adding a robot (a new line) or revoking
  one (deleting its line) needs no restart. A broken file lets no invited robot in, and is
  logged; robots with the shared token keep working.
- Invited robots get no server offers (`-offer`), which carry other servers' tokens.
- Browsers still pair only by the code on the robot's own screen, so an invite gives a robot
  a place on the server, not anyone access to it.
- Invited robots have send limits (above, "Limits").
- Still to do (see the [roadmap](docs/roadmap.md)): end-to-end encryption through a public
  server.

## Running in a container

A public instance runs at **https://chan.w42.eu**. To run your own:

1. **Image:** pushing a `v*` tag builds `ghcr.io/mj41/stackchan-server:<tag>`
   (`.github/workflows/release.yml`). Pin it by digest where you deploy it.
2. **Robot token:** mount the robot token file at `/secrets/robot-token` (keep it out of
   git), and pass `-public-url https://<your host>`.

What any host needs:

- **One instance:** state is in memory, so run exactly one, and stop the old one before
  the new one starts. The image disables the state file (`-state-file ""`), so a restart
  forgets pairings unless you mount a volume and pass `-state-file /state/state.json`.
- **No request timeout** on the proxy in front of it: robot WebSockets, the browser's
  event stream and the media sockets stay open for hours.
- **TLS** may end at the proxy: `-public-url https://…` also makes the session cookie
  `Secure`.
- **Audio bandwidth:** microphone audio is raw PCM (about 48 KB/s). Compress it before
  offering audio over the internet.

## Protocol

This is the device wire protocol v1. Its reference specification is [wire-protocol.md](https://github.com/mj41/home-w42-eu/blob/main/docs/wire-protocol.md) in home-w42-eu; the tables below are this server's view of it.

**Connect:** `ws://<server>/api/devices/connect` with these headers:

- `Authorization: Bearer <robot token>`
- `X-Device-Id: <robot id>`: 1–64 of `[A-Za-z0-9._-]`.

### JSON frames

Every WebSocket text message is one JSON object: `{"kind": "...", "meta": {...}, "body": {...}}`.

| Direction | Kind | Body |
|---|---|---|
| robot → server | `Register` (must be first) | `{"class": "robot", "capabilities": {"model", "firmware", "commands": [...], "measurements": [...]}}` |
| robot → server | `RobotTelemetry` | `{"measurements": {"battery_pct": 87.5}}` (values are numbers) |
| robot → server | `RobotEvent` | `{"name", "data"}`: `shake`, `head_press` with zone intensities `{"z0", "z1", "z2"}` (0–3), `head_release` with `{"ms"}`, `head_swipe_forward`, `head_swipe_backward`, `screen_tap` with `{"x", "y"}` (anywhere on the face, its eyes and mouth included), `screensaver_on` with `{"manual": 0 or 1}`, `screensaver_off`, `standby` with `{"minutes"}`, `standby_end` with `{"touched"}`, `nfc_tag` with `{"uid": "04:A2:…", "type", "atqa", "sak"}` plus `"text"` when the tag holds an NDEF URI or text record, `nfc_removed` with `{"uid"}`, `proximity_near` / `proximity_far` with `{"value"}`, `ir_received` with `{"protocol": "nec" or "raw", "address", "command", "raw"}` (raw marks/spaces in µs, always present), `screen_long_press` with `{"x", "y"}` (free for apps), `servers` with `{"list": JSON array of {name, url, origin, token: bool}, "current", "default"}`, `power_button` with `{"press": "short" or "long"}`, `usb_plugged` / `usb_unplugged`, `battery_inserted` / `battery_removed`. Values are numbers or strings. Browsers also get a per-robot `seq` |
| robot → server | `RobotPong` | `{"id", "queue_ms"}`: answer to `ping`; `queue_ms` is time spent waiting on the robot |
| robot → server | `Heartbeat` | `{}`, every 30 s |
| server → robot | `Accepted` / `Rejected` | `{}` / `{"reason": "..."}` |
| server → robot | `PairCode` | `{"code", "url", "expires_in_s"}`: show `url` as a QR code |
| server → robot | `Paired` | `{"viewers": 1}`: after a pairing, and right after `Accepted` when browsers are already paired (pairings survive restarts), so the robot starts with its face instead of the QR screen |
| server → robot | `RobotCommand` | `{"command": "nod", "args": {}}` |
| server → robot | `ServerOffer` | `{"servers": [{"name", "url", "token"?}]}`: other servers this server lets its robots switch to (from `-offer`), sent after `Accepted` |

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
| `leds` | `{"left": "#rrggbb", "right": "#rrggbb"}` fades a whole side. `{"pixels": [...]}` sets up to 12 single LEDs (left 0–5, right 6–11; `null` skips one). Left 0 sits at the screen end of its strip, and so does right 11 (the right strip runs from the back). `{"effect": "rainbow\|breathe\|chase\|blink\|off", "color", "speed": 0.2..5, "seconds"}` runs an animation on the robot (`seconds` 0 = until the next `leds`) |
| `brightness` | `{"value": 1..100}` sets it by hand and ends auto-brightness; `{"auto": bool}` switches brightness that follows the room light (robots with a light sensor report `auto_brightness`) |
| `volume` | `{"value": 0..100}` |
| `camera`, `mic` | `{"on": bool}`: sent by the server, not by browsers, while someone watches or listens |
| `ir_send` | `{"address", "command"}` sends an NEC code; `{"raw": "9000,4500,562,…", "carrier_hz": 38000}` sends marks and spaces in µs (e.g. what `ir_received` reported). `"frames": 1..5` sends the whole frame that many times, 108 ms apart (for weak links; a toggle button may toggle twice). `"repeat": 0..20` is like holding the button: NEC repeat codes every 108 ms, or the raw frame again after 40 ms. `"loopback": true` lets the robot hear its own signal (self-test) |
| `hold` | `{"seconds": 30..300}`: keep the head servos powered at the current angle (they go slack at rest otherwise); `0` releases now. Events `hold_on` `{"seconds"}` / `hold_off`, telemetry `hold_s` (seconds left) |
| `snapshot` | none: a 640x480 still (the robot switches the sensor for one frame), arriving as binary `0x07` |
| `camera_config` | `{"mirror": bool, "flip": bool}` |
| `camera_reg` | `{"reg": n}` reads, `{"reg": n, "value": v}` writes and reads back a raw GC0308 register; answer: event `camera_reg` `{"reg", "value"}` (−1 on failure) |
| `servo_power` | `{"on": bool}`: both head servos powered or limp. Events `servo_power_on` / `servo_power_off`, telemetry `servo_power` |
| `rotate` | `{"velocity": -1000..1000, "seconds": 1..30, "no_head_cable": true}`: continuous yaw rotation, refused (`rotate_refused` `{"reason"}`) without `no_head_cable`, stopped by `velocity: 0`, by any other head command, standby or the time limit. Events `rotate_on` / `rotate_off`, telemetry `rotate_s` |
| `assets` | none: the robot answers with an `assets` event, `list` = `{"files": [{"name", "bytes", "crc"}]}` (CRC-32 IEEE) plus `total` and `free` bytes |
| `asset_delete` | `{"name": "food/cake.png"}`: answered by `asset_deleted` or `asset_error` |
| `sprite` | `{"id", "asset", "x", "y", "scale", "angle", "opacity", "z", "hidden", "ms", "tap"}`: a stored picture (PNG with transparency, or JPEG) over the face; `x`, `y` is its center in screen pixels, `ms` glides it there; `tap: true` makes it a button: taps on it (not on its transparent parts) report `sprite`, `asset`, `sprite_x`, `sprite_y` in `screen_tap` / `screen_long_press` (menus); a new id needs `asset`; up to 24; `sprite_error` {id, reason} if it cannot be shown |
| `sprite_hide`, `sprite_clear` | `{"id"}` / none: remove one sprite / all |
| `picture` | `{"asset": "pet/dream.jpg"}`: a stored picture instead of the face (`face` ends it) |
| `play` | `{"asset": "snd/hello.wav", "volume": 0..100}`: a stored WAV (16-bit PCM, mono or stereo, 4-48 kHz), streamed from the file so any length works; `sound_done` {asset} at the end, `sound_error` {asset, reason}; `play_stop` ends it |
| `touch_stream`, `imu_stream`, `light_stream` | `{"on": bool}`: sent by the server while a browser has `?touch=1` / `?imu=1` / `?light=1` open (an app server such as [stackchan-pet](https://github.com/mj41/stackchan-pet) may send `light_stream` itself) |
| `server_add` | `{"url": "ws://…" or "wss://…", "name", "token"}`: add (or update) an entry in the robot's server list, stored on the robot |
| `server_remove`, `server_default`, `server_switch` | `{"server": url or name}`: remove an entry (not the built-in or current one), make it the one used at start (`""` = no default: the robot starts as a chooser), or switch to it now (the robot leaves this server) |
| `proximity` | `{"on": bool}`: the proximity sensor, whose IR LED next to the camera pulses ~10×/s. Off stops the LED and the approach events; light and auto-brightness keep working. Telemetry `proximity_on` |
| `power_led` | `{"mode": "on\|off\|blink\|fast\|charging"}`: the red power LED; `charging` hands it back to the charger |
| `nfc` | `{"on": bool}`: NFC tag polling, on by default. Listed only when the robot found its reader |
| `automation` | `{"autostart": bool}`: open Embody Mode after every power-on or restart (stored on the robot, off until set); the robot answers with an `automation {autostart}` event, also sent after it registers. Only with firmware built with `CONFIG_STACKCHAN_EMBODY_AUTOMATION` (off by default), like the next two |
| `restart` | none: restart the robot, back into Embody Mode |
| `launch` | `{"app": "AVATAR\|AI.AGENT\|DANCE\|SETUP\|…"}`: restart into another launcher app once (`""` = the launcher); `launch_unknown {app}` event for a name it does not have |
| `car_enable` | `{"on": bool, "board"?: "v1\|v2\|both"}`: the optional TPBot car (micro:bit with [tpbot-ble](https://github.com/mj41/tpbot-ble)) over BLE (firmware `CONFIG_STACKCHAN_EMBODY_CAR`). Off by default and kept on the robot. While on, the robot registers again with the `car_*` commands and telemetry listed in sbot's readme, [Car capability](https://github.com/mj41/sbot#car-capability), plus `car_connected`, and sends `car_connected` / `car_disconnected` events. This dashboard has no car controls; sbot has |

### Binary messages

The first byte is the type, followed by the payload.

| Type | Direction | Payload |
|---|---|---|
| `0x01` | robot → server → browser | camera frame, JPEG |
| `0x02` | robot → server → browser | microphone: sample rate (uint16 LE), then s16le mono PCM |
| `0x04` | robot → server → browser | microphone, all codec channels: sample rate (uint16 LE), channel count (uint8), then interleaved s16le PCM (on StackChan: channel 1 is the microphone, channel 0 the speaker reference, i.e. what the robot plays, looped back for echo cancellation; the dashboard plays either one) |
| `0x05` | robot → server → browser | raw IMU while a browser has `?imu=1` open: count (uint16 LE), then per sample time (uint32 LE ms) and 9 float32 LE: accel m/s², gyro °/s, magnetic µT |
| `0x06` | robot → server → browser | raw touch frames while a browser has `?touch=1` open: frame count (uint16 LE), then per frame time (uint32 LE ms), n (uint8) and n × (id uint8, x uint16 LE, y uint16 LE) |
| `0x08` | robot → server → browser | raw light and proximity every 50 ms while a browser has `?light=1` open: sample count (uint16 LE), then per sample time (uint32 LE ms), proximity (uint16 LE, 0..2047, 0 while off), light CH0 visible+IR and CH1 IR (uint16 LE raw counts); the sensor runs fast (proximity 50 ms, light 100 ms) only while streaming |
| `0x07` | robot → server | full-resolution JPEG still (the `snapshot` command); the server keeps the latest and serves it at `GET /api/robots/{id}/snapshot` (SSE `snapshot` `{"robot", "bytes", "ts"}`) |
| `0x03` | browser → server → robot | speaker: sample rate (uint16 LE), then s16le mono PCM. Sent on the media socket; forwarded only to robots that list `speaker` |
| `0x10` | server → robot | picture, JPEG 320x240, shown instead of the face |
| `0x11` | server → robot | part of a file for the robot's file store: name length (uint8), name (e.g. `food/cake.png`), total size and offset (uint32 LE each), data; sent in order in 32 KB chunks by `POST /api/robots/{id}/assets?name=…` (header `X-Stackchan-Upload: 1`, the file as the body, max 2 MB) |

### Browser API

All endpoints need the session cookie of a browser that paired with the robot.

| Endpoint | Purpose |
|---|---|
| `GET /pair?code=…` | QR target; adds the robot to this browser's session |
| `POST /api/join` | an unpaired browser asks paired browsers for access; returns `{"id", "code", "from", "agent", "expires"}`. Paired browsers get a `join_request` SSE event (also replayed when they connect) |
| `POST /api/join/{id}/approve`, `…/deny` | a paired browser answers; approve pairs the requester with the approver's robots. The requester gets `join_result` `{"status"}`, the other paired browsers `join_closed` |
| `GET /api/robots` | paired robots (JSON) |
| `GET /api/events` | SSE: `robot` (full state; telemetry `screensaver` 0 off / 1 auto / 2 manual), `robot_event` and `command_sent` (`{"robot", "seq", "command", "args", "ts"}`: what a browser sent; both go to every paired browser, and the last 40 are replayed on connect), `pong` (only to the browser that pinged), `join_request` / `join_closed` / `join_result` (see `/api/join`) |
| `POST /api/robots/{id}/command` | `{"command", "args"}` as JSON |
| `POST /api/robots/{id}/picture` | `image/jpeg` body, up to 192 KB |
| `GET /api/robots/{id}/media?video=1&audio=1&imu=1` | WebSocket, same origin only, carrying binary `0x01` (video), `0x02`/`0x04` (audio), `0x05` (imu) for what was asked; the server turns camera, mic and IMU stream on while at least one browser wants them |

**Liveness:** the server sends a WebSocket ping every 5 s and drops the robot after 60 s without traffic. Reconnect with backoff from 1 s up to 30 s.

**Pairing:** codes are one-time and valid for `-pair-ttl` (default 5 min). A used or expiring code is replaced by a fresh `PairCode`, so the QR on the robot's screen always works.

## Status

A prototype, in daily use with one robot on a home network since 2026-09-29. It is not
yet safe on untrusted networks: one shared robot token, no per-person permissions, plain
`ws://` on the LAN. The way to a v1, in stages with clear goals, is in
[docs/roadmap.md](docs/roadmap.md).

## Related projects

- [StackChan fork, branch `embody-mj41`](https://github.com/mj41/StackChan/tree/embody-mj41): the robot's firmware, Embody Mode.
- Other app servers the robot can switch to, on the same `wire` package: [stackchan-pet](https://github.com/mj41/stackchan-pet) (a Tamagotchi for kids) and [sbot](https://github.com/mj41/sbot) (a cockpit for the robot and a TPBot car, growing into the home node).
- [home-w42-eu](https://github.com/mj41/home-w42-eu): the platform, its use cases, architecture and the wire protocol's reference. All the repos: [The repos today](https://github.com/mj41/home-w42-eu#the-repos-today).
- [stackchan-mj](https://github.com/mj41/stackchan-mj): working notes, scripts to run this server in the background on a LAN, the robot's hardware coverage and the trust design behind the [roadmap](docs/roadmap.md).

## Credits

The dashboard's emoji are [Fluent Emoji](https://github.com/microsoft/fluentui-emoji) (Flat style) by Microsoft, MIT license; see [internal/server/ui/emoji/LICENSE](internal/server/ui/emoji/LICENSE).

## License

MIT, see [LICENSE](LICENSE). The bundled emoji are Microsoft's Fluent Emoji (MIT), see [internal/server/ui/emoji/LICENSE](internal/server/ui/emoji/LICENSE).
