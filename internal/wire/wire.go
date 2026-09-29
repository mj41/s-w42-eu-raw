// Package wire defines the robot <-> server WebSocket frames.
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
)

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
}

// RobotCommandBody is one command. The "basic" command set a Stack-chan offers:
//
//	ping       {"id": "..."}                  answered by RobotPong
//	nod, shake, home                           head gestures
//	look       {"yaw": deg, "pitch": deg}
//	emotion    {"name": "neutral|happy|angry|sad|doubt|sleepy"}
//	say        {"text": "...", "seconds": 6}   speech bubble
//	leds       {"left": "#rrggbb", "right": "#rrggbb"}
//	brightness {"value": 1..100}
//	volume     {"value": 0..100}
//	sticker    {"name": "heart|angry|sweat|shy|dizzy", "seconds": 3}  decoration over the face
//	face                                       back to the face after a picture
//	image                                      (no JSON: pictures arrive as BinShowJPEG)
//	camera     {"on": bool}                    sent by the server while browsers watch
//	mic        {"on": bool}                    sent by the server while browsers listen
type RobotCommandBody struct {
	Command string         `json:"command"`
	Args    map[string]any `json:"args,omitempty"`
}

// RobotEventBody reports something that happened on the robot: "shake",
// "head_press", "head_swipe_forward", "head_swipe_backward", or "screen_tap"
// with data {"x", "y"} in screen pixels (320x240).
type RobotEventBody struct {
	Name string         `json:"name"`
	Data map[string]any `json:"data,omitempty"`
}

// Binary WebSocket messages: one type byte, then the payload.
const (
	BinCameraJPEG byte = 0x01 // robot -> server -> browser: camera frame (JPEG)
	BinAudioPCM   byte = 0x02 // robot -> server -> browser: microphone, uint16 LE sample rate then s16le mono PCM
	BinShowJPEG   byte = 0x10 // server -> robot: picture (JPEG, 320x240) shown instead of the face
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
