package server

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mj41/stackchan-server/internal/wire"
)

const testToken = "test-token"

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	s := New(Config{RobotToken: testToken, PairTTL: time.Minute, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(s.Handler())
	s.cfg.PublicURL = ts.URL
	t.Cleanup(ts.Close)
	return ts, s
}

type testRobot struct {
	t  *testing.T
	ws *websocket.Conn
}

func dialRobot(t *testing.T, ts *httptest.Server, token, id string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set(wire.WorkerIDHeader, id)
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+wire.ConnectPath, h)
}

func connectRobot(t *testing.T, ts *httptest.Server, id string, class string) *testRobot {
	t.Helper()
	ws, _, err := dialRobot(t, ts, testToken, id)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	r := &testRobot{t: t, ws: ws}
	r.send(wire.KindRegister, wire.RegisterBody{
		Class:        class,
		Capabilities: wire.RobotCapabilities{Model: "test", Commands: []string{"nod"}},
	})
	return r
}

func (r *testRobot) send(kind string, body any) {
	r.t.Helper()
	f, err := wire.Marshal(kind, wire.Meta{}, body)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.ws.WriteMessage(websocket.TextMessage, f); err != nil {
		r.t.Fatalf("robot write: %v", err)
	}
}

// expect reads frames until one of the given kind arrives and decodes its body.
func (r *testRobot) expect(kind string, body any) {
	r.t.Helper()
	r.ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := r.ws.ReadMessage()
		if err != nil {
			r.t.Fatalf("waiting for %s: %v", kind, err)
		}
		frames, err := wire.Parse(data)
		if err != nil {
			r.t.Fatal(err)
		}
		for _, f := range frames {
			if f.Kind == kind {
				if err := f.Decode(body); err != nil {
					r.t.Fatal(err)
				}
				return
			}
		}
	}
}

func newBrowser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 5 * time.Second}
}

func TestPairTelemetryAndCommand(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "chan-1", wire.ClassRobot)

	var accepted struct{}
	robot.expect(wire.KindAccepted, &accepted)
	var pc wire.PairCodeBody
	robot.expect(wire.KindPairCode, &pc)
	if !strings.HasPrefix(pc.URL, ts.URL+"/pair?code=") || len(pc.Code) != 8 {
		t.Fatalf("unexpected pair code %+v", pc)
	}

	browser := newBrowser(t)

	// Before pairing the browser sees nothing and cannot command the robot.
	if got := listRobots(t, browser, ts); len(got) != 0 {
		t.Fatalf("unpaired browser sees robots: %+v", got)
	}
	if code := postCommand(t, browser, ts, "chan-1", "nod"); code != http.StatusForbidden {
		t.Fatalf("unpaired command status = %d, want 403", code)
	}

	// Scan the QR.
	resp, err := browser.Get(pc.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Query().Get("paired") != "chan-1" {
		t.Fatalf("pair: status %d, final URL %s", resp.StatusCode, resp.Request.URL)
	}
	var paired wire.PairedBody
	robot.expect(wire.KindPaired, &paired)
	if paired.Viewers != 1 {
		t.Fatalf("viewers = %d, want 1", paired.Viewers)
	}
	var next wire.PairCodeBody
	robot.expect(wire.KindPairCode, &next)
	if next.Code == pc.Code {
		t.Fatal("pairing code was not replaced after use")
	}

	// A used code does not work again, e.g. for someone who photographed the QR.
	resp, err = newBrowser(t).Get(pc.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reused code status = %d, want 400", resp.StatusCode)
	}

	// Telemetry reaches the paired browser over SSE.
	events := openEvents(t, browser, ts)
	robot.send(wire.KindRobotTelemetry, wire.RobotTelemetryBody{Measurements: map[string]float64{"battery_pct": 42}})
	waitFor(t, events, "telemetry", func(v robotView) bool {
		return v.ID == "chan-1" && v.Online && v.Telemetry["battery_pct"] == 42
	})

	// Commands reach the robot; unsupported ones are refused.
	if code := postCommand(t, browser, ts, "chan-1", "nod"); code != http.StatusAccepted {
		t.Fatalf("command status = %d, want 202", code)
	}
	var cmd wire.RobotCommandBody
	robot.expect(wire.KindRobotCommand, &cmd)
	if cmd.Command != "nod" {
		t.Fatalf("robot got command %q", cmd.Command)
	}
	if code := postCommand(t, browser, ts, "chan-1", "self-destruct"); code != http.StatusBadRequest {
		t.Fatalf("unsupported command status = %d, want 400", code)
	}

	// Disconnect shows the robot offline and commands fail.
	robot.ws.Close()
	waitFor(t, events, "offline state", func(v robotView) bool { return v.ID == "chan-1" && !v.Online })
	if code := postCommand(t, browser, ts, "chan-1", "nod"); code != http.StatusConflict {
		t.Fatalf("offline command status = %d, want 409", code)
	}
}

func TestRobotAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	if _, resp, err := dialRobot(t, ts, "wrong", "chan-1"); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: err=%v resp=%v", err, resp)
	}
	if _, resp, err := dialRobot(t, ts, testToken, "bad id!"); err == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad id: err=%v resp=%v", err, resp)
	}
}

func TestRejectsNonRobotClass(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "thermo-1", "sensor")
	var rej wire.RejectedBody
	robot.expect(wire.KindRejected, &rej)
	if rej.Reason != "unknown worker class" {
		t.Fatalf("reason = %q", rej.Reason)
	}
}

func TestInvalidPairCode(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := newBrowser(t).Get(ts.URL + "/pair?code=NOPE2345")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func listRobots(t *testing.T, c *http.Client, ts *httptest.Server) []robotView {
	t.Helper()
	resp, err := c.Get(ts.URL + "/api/robots")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var views []robotView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	return views
}

func postCommand(t *testing.T, c *http.Client, ts *httptest.Server, id, command string) int {
	t.Helper()
	resp, err := c.Post(ts.URL+"/api/robots/"+id+"/command", "application/json",
		strings.NewReader(`{"command":"`+command+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitFor(t *testing.T, events <-chan robotView, what string, match func(robotView) bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case v := <-events:
			if match(v) {
				return
			}
		case <-deadline:
			t.Fatalf("%s did not reach the browser", what)
		}
	}
}

// openEvents streams decoded "robot" SSE events until the test ends.
func openEvents(t *testing.T, c *http.Client, ts *httptest.Server) <-chan robotView {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	stream := &http.Client{Jar: c.Jar} // no timeout: the stream stays open
	resp, err := stream.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	out := make(chan robotView, 64)
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if line == ": ready" {
				close(ready)
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var v robotView
				if json.Unmarshal([]byte(data), &v) == nil {
					out <- v
				}
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE stream not ready")
	}
	return out
}
