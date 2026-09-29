// Command fake-robot plays the Stack-chan side of the protocol so the server
// and dashboard can be tested without hardware: it registers, prints the
// pairing URL, streams fake telemetry, and logs commands.
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

func main() {
	var (
		serverURL = flag.String("server", "ws://127.0.0.1:8765", "server base URL (ws:// or wss://)")
		id        = flag.String("id", "fake-robot-1", "robot id")
		tokenFile = flag.String("token-file", defaultTokenFile(), "file with the robot bearer token")
		interval  = flag.Duration("telemetry", time.Second, "telemetry interval")
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

	// Reconnect with exponential backoff 1 s -> 30 s, like yolovm workers.
	backoff := time.Second
	for {
		start := time.Now()
		err := run(url, token, *id, *interval, log)
		log.Warn("disconnected", "err", err)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

func run(url, token, id string, interval time.Duration, log *slog.Logger) error {
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

	reg, _ := wire.Marshal(wire.KindRegister, wire.Meta{WorkerID: id}, wire.RegisterBody{
		Class: wire.ClassRobot,
		Capabilities: wire.RobotCapabilities{
			Model:        "fake",
			Firmware:     "fake-robot",
			Commands:     []string{"nod", "shake"},
			Measurements: []string{"battery_pct", "head_pitch_deg", "temperature_c"},
		},
	})
	if err := ws.WriteMessage(websocket.TextMessage, reg); err != nil {
		return err
	}

	// gorilla allows one concurrent writer: the reader only logs, all writes happen here.
	errc := make(chan error, 1)
	go func() { errc <- readLoop(ws, log) }()

	telemetry := time.NewTicker(interval)
	defer telemetry.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	battery, pitch := 87.0, 0.0
	for {
		select {
		case err := <-errc:
			return err
		case <-telemetry.C:
			battery = max(0, battery-0.01)
			pitch = max(-15, min(15, pitch+rand.NormFloat64()))
			f, _ := wire.Marshal(wire.KindRobotTelemetry, wire.Meta{}, wire.RobotTelemetryBody{
				Measurements: map[string]float64{
					"battery_pct":    round1(battery),
					"head_pitch_deg": round1(pitch),
					"temperature_c":  round1(24 + rand.Float64()),
				},
			})
			if err := ws.WriteMessage(websocket.TextMessage, f); err != nil {
				return err
			}
		case <-heartbeat.C:
			f, _ := wire.Marshal(wire.KindHeartbeat, wire.Meta{}, nil)
			if err := ws.WriteMessage(websocket.TextMessage, f); err != nil {
				return err
			}
		}
	}
}

func readLoop(ws *websocket.Conn, log *slog.Logger) error {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
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
				log.Info("command received", "command", b.Command, "args", b.Args)
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
