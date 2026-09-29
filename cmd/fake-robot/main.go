// Command fake-robot plays the Stack-chan side of the protocol so the server
// and dashboard can be tested without hardware: it registers, prints the
// pairing URL, streams telemetry, answers the basic command set (including
// ping), and emits an occasional robot event.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
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
var commands = []string{"ping", "nod", "shake", "look", "home", "emotion", "say", "leds", "brightness", "volume"}

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
				"wifi_rssi_dbm", "free_heap_kb", "uptime_s", "brightness_pct", "volume_pct"},
		},
	}
	if err := send(wire.KindRegister, reg); err != nil {
		return err
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
	fakeEvents := []string{"head_press", "head_swipe_forward", "shake", "pickup"}

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
			name := fakeEvents[rand.IntN(len(fakeEvents))]
			r.log.Info("robot event", "name", name)
			err = send(wire.KindRobotEvent, wire.RobotEventBody{Name: name})
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
			r.pitch = max(3, min(87, v))
		}
	case "home":
		r.yaw, r.pitch = 0, 45
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
	}
}

func readLoop(ws *websocket.Conn, cmds chan<- received, log *slog.Logger) error {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		at := time.Now()
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
