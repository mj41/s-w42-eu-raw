// Package wire defines the robot <-> server WebSocket frames. It is public so other
// servers the robot can switch to (e.g. stackchan-pet) speak the same protocol.
//
// Every frame is a JSON object {"kind", "meta", "body"}, the same envelope as
// yolovm-pilot (~/work-stai/stai-yolovm/docs/yolovm-pilot/wire-protocol.md),
// so a robot speaking this protocol can also register with that pilot as a
// "robot"-class worker. Frames stay flat and small so the ESP-IDF side can
// handle them with cJSON.
package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Connection handshake, shared with yolovm-pilot.
const (
	ConnectPath    = "/api/workers/connect"
	WorkerIDHeader = "X-Yolovm-Worker-Id"
)

// Frame kinds.
const (
	// Robot -> server.
	KindRegister       = "Register"
	KindHeartbeat      = "Heartbeat"
	KindRobotTelemetry = "RobotTelemetry"
	KindRobotEvent     = "RobotEvent"
	KindRobotPong      = "RobotPong"

	// Server -> robot.
	KindAccepted     = "Accepted"
	KindRejected     = "Rejected"
	KindPairCode     = "PairCode"
	KindPaired       = "Paired"
	KindRobotCommand = "RobotCommand"
	KindServerOffer  = "ServerOffer"
)

// ServerOfferBody lists other servers this server lets its robots switch to
// (voluntary): the robot adds them to its server list. Token is the robot
// token for that server, when this server's operator may hand it out.
type ServerOfferBody struct {
	Servers []OfferedServer `json:"servers"`
}

type OfferedServer struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// ClassRobot is the only worker class this server accepts.
const ClassRobot = "robot"

// Frame is the envelope of every message.
type Frame struct {
	Kind string          `json:"kind"`
	Meta Meta            `json:"meta"`
	Body json.RawMessage `json:"body"`
}

// Meta holds routing and correlation fields.
type Meta struct {
	WorkerID  string `json:"worker_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	TS        string `json:"ts,omitempty"`
}

type RegisterBody struct {
	Class        string            `json:"class"`
	Labels       map[string]string `json:"labels,omitempty"`
	Capabilities RobotCapabilities `json:"capabilities"`
}

// RobotCapabilities is the "robot" class capability schema.
type RobotCapabilities struct {
	Model        string   `json:"model,omitempty"`        // e.g. "stackchan-cores3"
	Firmware     string   `json:"firmware,omitempty"`     // firmware version string
	Commands     []string `json:"commands,omitempty"`     // commands the robot accepts, e.g. ["nod"]
	Measurements []string `json:"measurements,omitempty"` // telemetry keys it sends
}

type RejectedBody struct {
	Reason string `json:"reason"`
}

type RobotTelemetryBody struct {
	Measurements map[string]float64 `json:"measurements"`
}

// PairCodeBody carries a one-time pairing code. The robot shows URL as a QR code.
type PairCodeBody struct {
	Code       string `json:"code"`
	URL        string `json:"url"`
	ExpiresInS int    `json:"expires_in_s"`
}

// PairedBody tells the robot a browser just paired; Viewers is the total count.
type PairedBody struct {
	Viewers int `json:"viewers"`
	// Reconnect: sent right after Accepted because browsers were paired before, not
	// because someone just scanned. A robot showing its QR on purpose may keep it.
	Reconnect bool `json:"reconnect,omitempty"`
}

// RobotCommandBody is one command. The "basic" command set a Stack-chan offers:
//
//	ping       {"id": "..."}                  answered by RobotPong
//	nod, shake, home                           head gestures
//	look       {"yaw": deg, "pitch": deg}
//	emotion    {"name": "neutral|happy|angry|sad|doubt|sleepy"}
//	say        {"text": "...", "seconds": 6}   speech bubble
//	leds       {"left": "#rrggbb", "right": "#rrggbb"}  fade a side; also
//	           {"pixels": ["#rrggbb" or null, ...]}   12 single LEDs, left 0-5, right 6-11
//	           {"effect": "rainbow|breathe|chase|blink|off", "color", "speed": 0.2..5, "seconds"}
//	brightness {"value": 1..100} by hand (ends auto), or {"auto": bool} to follow the room light
//	volume     {"value": 0..100}
//	sticker    {"name": "heart|angry|sweat|shy|dizzy", "seconds": 3}  decoration over the face
//	face                                       back to the face after a picture
//	image                                      (no JSON: pictures arrive as BinShowJPEG)
//	camera     {"on": bool}                    sent by the server while browsers watch
//	mic        {"on": bool}                    sent by the server while browsers listen
//	imu_stream {"on": bool}                    sent by the server while browsers want BinIMU
//	touch_stream {"on": bool}                  sent by the server while browsers want BinTouch
//	light_stream {"on": bool}                  sent while browsers (or an app) want BinLight
//	servo_power {"on": bool}                   power both head servos (off: limp)
//	rotate     {"velocity": -1000..1000, "seconds": 1..30, "no_head_cable": true}  continuous yaw
//	snapshot                                   full-resolution still, arrives as BinSnapshot
//	camera_config {"mirror": bool, "flip": bool}
//	camera_reg {"reg": n, "value": v}          raw sensor register write (optional) and read
//	server_add {"url", "name", "token"}        add/update an entry in the robot's server list
//	server_remove / server_default / server_switch {"server": url or name}
//	screensaver {"on": bool}                   blank the screen / wake it
//	standby    {"minutes": 1..120}             offline with the screen off
//	speaker                                    (no JSON: audio arrives as BinSpeakerPCM)
//	nfc        {"on": bool}                    NFC tag polling (on by default)
//	ir_send    {"address", "command"} (NEC) or {"raw": "9000,4500,...", "carrier_hz"}  infrared, us mark/space
//	power_led  {"mode": "on|off|blink|fast|charging"}  the red power/charge LED
//	proximity  {"on": bool}                    proximity sensor (its IR LED); on after boot
//	hold       {"seconds": 30..300}            keep the head servos powered (0 = release now)
type RobotCommandBody struct {
	Command string         `json:"command"`
	Args    map[string]any `json:"args,omitempty"`
}

// RobotEventBody reports something that happened on the robot: "shake",
// "head_press" with the zone intensities {"z0", "z1", "z2"} (0-3),
// "head_release" with {"ms"}, "head_swipe_forward", "head_swipe_backward", "screen_tap"
// with data {"x", "y"} in screen pixels (320x240), "nfc_tag" with data
// {"uid", "type", "atqa", "sak", optional "text"} (the first NDEF record: URI
// or text), "nfc_removed" with {"uid"}, "proximity_near" / "proximity_far"
// with {"value"} (someone came close / left), "ir_received" with {"protocol":
// "nec" or "raw", "address", "command" (NEC), "raw": "mark,space,..." (us)},
// "power_button" with {"press": "short" or "long"}, "usb_plugged", "usb_unplugged",
// "battery_inserted", "battery_removed", "hold_on" {"seconds"} / "hold_off",
// and screensaver/standby events.
type RobotEventBody struct {
	Name string         `json:"name"`
	Data map[string]any `json:"data,omitempty"`
}

// Binary WebSocket messages: one type byte, then the payload.
const (
	BinCameraJPEG byte = 0x01 // robot -> server -> browser: camera frame (JPEG)
	BinAudioPCM   byte = 0x02 // robot -> server -> browser: microphone, uint16 LE sample rate then s16le mono PCM
	BinSpeakerPCM byte = 0x03 // browser -> server -> robot: speaker, same layout as BinAudioPCM
	// BinAudioMulti: robot -> server -> browser: microphone, all codec channels:
	// uint16 LE sample rate, uint8 channel count, then interleaved s16le PCM.
	BinAudioMulti byte = 0x04
	// BinIMU: robot -> server -> browser: raw IMU samples (100 Hz) while a browser asks
	// for them: uint16 LE count, then per sample uint32 LE time (ms) and 9 float32 LE:
	// accel x/y/z (m/s^2), gyro x/y/z (deg/s), magnetic x/y/z (uT).
	BinIMU byte = 0x05
	// BinTouch: robot -> server -> browser: raw touch frames (every 20 ms) while a browser
	// asks for them: uint16 LE frame count, then per frame uint32 LE time (ms), uint8 n and
	// n x (uint8 id, uint16 LE x, uint16 LE y).
	BinTouch byte = 0x06
	// BinLight: robot -> server -> browser: raw light and proximity samples (every 50 ms)
	// while a browser asks for them (light_stream): uint16 LE count, then per sample
	// uint32 LE time (ms), uint16 LE proximity (0..2047, 0 while proximity is off),
	// uint16 LE light CH0 (visible + IR) and CH1 (IR), raw counts.
	BinLight byte = 0x08
	// BinSnapshot: robot -> server: a full-resolution JPEG still (the "snapshot" command).
	// The server keeps the latest per robot and serves it at /api/robots/{id}/snapshot.
	BinSnapshot byte = 0x07
	BinShowJPEG byte = 0x10 // server -> robot: picture (JPEG, 320x240) shown instead of the face
)

// RobotPongBody answers the "ping" command. QueueMs is how long the ping
// waited on the robot between arriving and being handled.
type RobotPongBody struct {
	ID      string  `json:"id"`
	QueueMs float64 `json:"queue_ms"`
}

// Marshal builds one frame. A nil body is sent as {}.
func Marshal(kind string, meta Meta, body any) ([]byte, error) {
	if meta.TS == "" {
		meta.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	raw := json.RawMessage("{}")
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal %s body: %w", kind, err)
		}
		raw = b
	}
	return json.Marshal(Frame{Kind: kind, Meta: meta, Body: raw})
}

// Parse decodes one WebSocket message: a single frame or, as yolovm-pilot
// allows for worker -> server, a JSON array of frames.
func Parse(data []byte) ([]Frame, error) {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var frames []Frame
		if err := json.Unmarshal(data, &frames); err != nil {
			return nil, fmt.Errorf("parse frame batch: %w", err)
		}
		return frames, nil
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse frame: %w", err)
	}
	return []Frame{f}, nil
}

// Decode unmarshals the frame body into v. An empty body leaves v unchanged.
func (f Frame) Decode(v any) error {
	if len(f.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(f.Body, v); err != nil {
		return fmt.Errorf("decode %s body: %w", f.Kind, err)
	}
	return nil
}
