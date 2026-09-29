// Command fake-robot plays the Stack-chan side of the protocol so the server
// and dashboard can be tested without hardware: it registers, prints the
// pairing URL, streams telemetry, answers the basic command set (including
// ping), and emits an occasional robot event.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mj41/stackchan-server/internal/wire"
)

// Same command set as the firmware (see wire.RobotCommandBody).
var commands = []string{"ping", "nod", "shake", "look", "home", "emotion", "say", "leds", "brightness", "volume",
	"sticker", "face", "image", "camera", "mic", "screensaver", "standby"}

func main() {
	var (
		serverURL = flag.String("server", "ws://127.0.0.1:8765", "server base URL (ws:// or wss://)")
		id        = flag.String("id", "fake-robot-1", "robot id")
		tokenFile = flag.String("token-file", defaultTokenFile(), "file with the robot bearer token")
		interval  = flag.Duration("telemetry", 2*time.Second, "telemetry interval")
		events    = flag.Duration("events", 20*time.Second, "interval of fake robot events (0 disables)")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	b, err := os.ReadFile(*tokenFile)
	if err != nil {
		log.Error("read token (start stackchan-server once to generate it)", "err", err)
		os.Exit(1)
	}
	token := strings.TrimSpace(string(b))
	url := strings.TrimRight(*serverURL, "/") + wire.ConnectPath

	r := &robot{started: time.Now(), battery: 87, pitch: 45, brightness: 60, volume: 50, log: log}
	// Reconnect with exponential backoff 1 s -> 30 s, like yolovm workers.
	backoff := time.Second
	for {
		start := time.Now()
		err := r.run(url, token, *id, *interval, *events)
		var sb standbyError
		if errors.As(err, &sb) {
			log.Info("standby: offline", "for", sb.d)
			time.Sleep(sb.d)
			r.afterStandby = true
			backoff = time.Second
			continue
		}
		log.Warn("disconnected", "err", err)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

// robot is the simulated state that commands change and telemetry reports.
type robot struct {
	started             time.Time
	battery, yaw, pitch float64
	brightness, volume  float64
	cameraOn, micOn     bool
	screensaver         float64 // 0 off, 1 auto, 2 manual
	afterStandby        bool    // report standby_end after the next connect
	frame               int     // camera frames sent, drives the test pattern
	phase               float64 // microphone tone phase
	log                 *slog.Logger
}

type received struct {
	cmd wire.RobotCommandBody
	at  time.Time
}

func (r *robot) run(url, token, id string, interval, eventEvery time.Duration) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set(wire.WorkerIDHeader, id)
	ws, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial: %w (HTTP %s)", err, resp.Status)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer ws.Close()

	send := func(kind string, body any) error {
		f, err := wire.Marshal(kind, wire.Meta{}, body)
		if err != nil {
			return err
		}
		return ws.WriteMessage(websocket.TextMessage, f)
	}

	reg := wire.RegisterBody{
		Class: wire.ClassRobot,
		Capabilities: wire.RobotCapabilities{
			Model:    "fake",
			Firmware: "fake-robot",
			Commands: commands,
			Measurements: []string{"battery_pct", "charging", "head_yaw_deg", "head_pitch_deg",
				"wifi_rssi_dbm", "free_heap_kb", "uptime_s", "brightness_pct", "volume_pct", "screensaver"},
		},
	}
	if err := send(wire.KindRegister, reg); err != nil {
		return err
	}
	if r.afterStandby {
		r.afterStandby, r.screensaver = false, 0
		if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "standby_end", Data: map[string]any{"touched": 0}}); err != nil {
			return err
		}
	}

	// gorilla allows one concurrent writer: the reader hands commands to this loop.
	errc := make(chan error, 1)
	cmds := make(chan received, 16)
	go func() { errc <- readLoop(ws, cmds, r.log) }()

	telemetry := time.NewTicker(interval)
	defer telemetry.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	var eventTick <-chan time.Time
	if eventEvery > 0 {
		t := time.NewTicker(eventEvery)
		defer t.Stop()
		eventTick = t.C
	}
	fakeEvents := []string{"head_press", "head_swipe_forward", "shake", "screen_tap"}
	videoTick := time.NewTicker(200 * time.Millisecond) // 5 fps, like the firmware
	defer videoTick.Stop()
	audioTick := time.NewTicker(40 * time.Millisecond) // 40 ms PCM chunks
	defer audioTick.Stop()
	sendBinary := func(msg []byte) error { return ws.WriteMessage(websocket.BinaryMessage, msg) }

	for {
		var err error
		select {
		case err = <-errc:
			return err
		case c := <-cmds:
			err = r.handle(c, send)
		case <-telemetry.C:
			r.battery = max(0, r.battery-0.01)
			err = send(wire.KindRobotTelemetry, wire.RobotTelemetryBody{Measurements: r.telemetry()})
		case <-heartbeat.C:
			err = send(wire.KindHeartbeat, nil)
		case <-eventTick:
			ev := wire.RobotEventBody{Name: fakeEvents[rand.IntN(len(fakeEvents))]}
			if ev.Name == "screen_tap" {
				ev.Data = map[string]any{"x": rand.IntN(320), "y": rand.IntN(240)}
			}
			r.log.Info("robot event", "name", ev.Name, "data", ev.Data)
			err = send(wire.KindRobotEvent, ev)
		case <-videoTick.C:
			if r.cameraOn {
				err = sendBinary(r.cameraFrame())
			}
		case <-audioTick.C:
			if r.micOn {
				err = sendBinary(r.micChunk(40 * time.Millisecond))
			}
		}
		if err != nil {
			return err
		}
	}
}

func (r *robot) handle(c received, send func(string, any) error) error {
	num := func(key string) (float64, bool) {
		v, ok := c.cmd.Args[key].(float64)
		return v, ok
	}
	switch c.cmd.Command {
	case "ping":
		id, _ := c.cmd.Args["id"].(string)
		return send(wire.KindRobotPong, wire.RobotPongBody{
			ID: id, QueueMs: float64(time.Since(c.at).Microseconds()) / 1000,
		})
	case "look":
		if v, ok := num("yaw"); ok {
			r.yaw = max(-128, min(128, v))
		}
		if v, ok := num("pitch"); ok {
			r.pitch = max(5, min(85, v))
		}
	case "home":
		r.yaw, r.pitch = 0, 45
	case "standby":
		// Like the firmware: report it, then go offline for the given minutes
		// (fractions allowed here, for quick tests with curl).
		minutes, ok := num("minutes")
		if !ok || minutes <= 0 {
			minutes = 5
		}
		r.cameraOn, r.micOn, r.screensaver = false, false, 2
		if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "standby", Data: map[string]any{"minutes": minutes}}); err != nil {
			return err
		}
		return standbyError{time.Duration(minutes * float64(time.Minute))}
	case "screensaver":
		// Like the firmware: the command blanks "manually" (2) or wakes (0).
		on, _ := c.cmd.Args["on"].(bool)
		switch {
		case on && r.screensaver == 0:
			r.screensaver = 2
			if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "screensaver_on", Data: map[string]any{"manual": 1}}); err != nil {
				return err
			}
		case !on && r.screensaver != 0:
			r.screensaver = 0
			if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "screensaver_off"}); err != nil {
				return err
			}
		}
	case "camera":
		r.cameraOn, _ = c.cmd.Args["on"].(bool)
	case "mic":
		r.micOn, _ = c.cmd.Args["on"].(bool)
	case "brightness":
		if v, ok := num("value"); ok {
			r.brightness = max(1, min(100, v))
		}
	case "volume":
		if v, ok := num("value"); ok {
			r.volume = max(0, min(100, v))
		}
	}
	r.log.Info("command received", "command", c.cmd.Command, "args", c.cmd.Args)
	return send(wire.KindRobotTelemetry, wire.RobotTelemetryBody{Measurements: r.telemetry()})
}

func (r *robot) telemetry() map[string]float64 {
	return map[string]float64{
		"battery_pct":    round1(r.battery),
		"charging":       0,
		"head_yaw_deg":   round1(r.yaw),
		"head_pitch_deg": round1(r.pitch),
		"wifi_rssi_dbm":  float64(-55 - rand.IntN(10)),
		"free_heap_kb":   float64(180 + rand.IntN(20)),
		"uptime_s":       float64(int(time.Since(r.started).Seconds())),
		"brightness_pct": r.brightness,
		"volume_pct":     r.volume,
		"screensaver":    r.screensaver,
	}
}

func readLoop(ws *websocket.Conn, cmds chan<- received, log *slog.Logger) error {
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		at := time.Now()
		if kind == websocket.BinaryMessage {
			if len(data) > 1 && data[0] == wire.BinShowJPEG {
				cfg, err := jpeg.DecodeConfig(bytes.NewReader(data[1:]))
				log.Info("picture received", "bytes", len(data)-1, "width", cfg.Width, "height", cfg.Height, "err", err)
			}
			continue
		}
		frames, err := wire.Parse(data)
		if err != nil {
			log.Warn("bad frame", "err", err)
			continue
		}
		for _, f := range frames {
			switch f.Kind {
			case wire.KindAccepted:
				log.Info("registered")
			case wire.KindRejected:
				var b wire.RejectedBody
				f.Decode(&b)
				return errors.New("rejected: " + b.Reason)
			case wire.KindPairCode:
				var b wire.PairCodeBody
				f.Decode(&b)
				log.Info("pair by opening this URL (the robot would show it as a QR)", "url", b.URL, "expires_in_s", b.ExpiresInS)
			case wire.KindPaired:
				var b wire.PairedBody
				f.Decode(&b)
				log.Info("browser paired", "viewers", b.Viewers)
			case wire.KindRobotCommand:
				var b wire.RobotCommandBody
				f.Decode(&b)
				cmds <- received{cmd: b, at: at}
			default:
				log.Debug("ignoring frame", "kind", f.Kind)
			}
		}
	}
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

func defaultTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "robot-token"
	}
	return filepath.Join(dir, "stackchan-server", "robot-token")
}

// cameraFrame is a 320x240 test pattern: a gradient with a moving square.
func (r *robot) cameraFrame() []byte {
	r.frame++
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / 320), uint8(y * 255 / 240), uint8(r.frame * 8), 255})
		}
	}
	sx := (r.frame * 12) % 280
	for y := 100; y < 140; y++ {
		for x := sx; x < sx+40; x++ {
			img.Set(x, y, color.White)
		}
	}
	var buf bytes.Buffer
	buf.WriteByte(wire.BinCameraJPEG)
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50})
	return buf.Bytes()
}

// micChunk is a quiet 440 Hz tone: sample rate (uint16 LE), then s16le mono PCM.
func (r *robot) micChunk(d time.Duration) []byte {
	const rate = 16000
	n := int(d.Seconds() * rate)
	out := make([]byte, 1, 3+2*n)
	out[0] = wire.BinAudioPCM
	out = binary.LittleEndian.AppendUint16(out, rate)
	for i := 0; i < n; i++ {
		s := int16(3000 * math.Sin(r.phase))
		r.phase += 2 * math.Pi * 440 / rate
		out = binary.LittleEndian.AppendUint16(out, uint16(s))
	}
	return out
}

// standbyError ends a connection on purpose: stay offline for d, then reconnect.
type standbyError struct{ d time.Duration }

func (e standbyError) Error() string { return fmt.Sprintf("standby for %v", e.d) }
