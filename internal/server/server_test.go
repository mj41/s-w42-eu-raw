package server

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
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

func dialRobot(ts *httptest.Server, token, id string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set(wire.WorkerIDHeader, id)
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+wire.ConnectPath, h)
}

func connectRobot(t *testing.T, ts *httptest.Server, id string, class string) *testRobot {
	t.Helper()
	ws, _, err := dialRobot(ts, testToken, id)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	r := &testRobot{t: t, ws: ws}
	r.send(wire.KindRegister, wire.RegisterBody{
		Class:        class,
		Capabilities: wire.RobotCapabilities{Model: "test", Commands: []string{"nod", "ping", "camera", "mic", "image", "speaker"}},
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
		msgType, data, err := r.ws.ReadMessage()
		if err != nil {
			r.t.Fatalf("waiting for %s: %v", kind, err)
		}
		if msgType == websocket.BinaryMessage {
			continue
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

func newBrowser() *http.Client {
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

	browser := newBrowser()

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
	resp, err = newBrowser().Get(pc.URL)
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
	if _, resp, err := dialRobot(ts, "wrong", "chan-1"); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: err=%v resp=%v", err, resp)
	}
	if _, resp, err := dialRobot(ts, testToken, "bad id!"); err == nil || resp.StatusCode != http.StatusBadRequest {
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
	resp, err := newBrowser().Get(ts.URL + "/pair?code=NOPE2345")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSecureCookieBehindTLSGateway(t *testing.T) {
	for _, tc := range []struct {
		publicURL string
		secure    bool
	}{
		{"https://chan.example.com", true},
		{"http://192.168.1.10:8765", false},
	} {
		s := New(Config{RobotToken: testToken, PublicURL: tc.publicURL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Secure != tc.secure {
			t.Errorf("public URL %s: cookies %+v, want one with Secure=%v", tc.publicURL, cookies, tc.secure)
		}
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

func waitFor(t *testing.T, events <-chan sseMsg, what string, match func(robotView) bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-events:
			var v robotView
			if m.name == "robot" && json.Unmarshal(m.data, &v) == nil && match(v) {
				return
			}
		case <-deadline:
			t.Fatalf("%s did not reach the browser", what)
		}
	}
}

// sseMsg is one received Server-Sent Event.
type sseMsg struct {
	name string
	data []byte
}

// waitForEvent waits for an SSE event of the given name and decodes it.
func waitForEvent(t *testing.T, events <-chan sseMsg, name string) map[string]any {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-events:
			if m.name == name {
				var v map[string]any
				if err := json.Unmarshal(m.data, &v); err != nil {
					t.Fatal(err)
				}
				return v
			}
		case <-deadline:
			t.Fatalf("no %q event reached the browser", name)
		}
	}
}

// openEvents streams SSE events until the test ends.
func openEvents(t *testing.T, c *http.Client, ts *httptest.Server) <-chan sseMsg {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	stream := &http.Client{Jar: c.Jar} // no timeout: the stream stays open
	resp, err := stream.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	out := make(chan sseMsg, 64)
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(resp.Body)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			if line == ": ready" {
				close(ready)
			}
			if n, ok := strings.CutPrefix(line, "event: "); ok {
				name = n
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				out <- sseMsg{name: name, data: []byte(data)}
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

// pairBrowser scans the robot's current QR code with a fresh browser.
func pairBrowser(t *testing.T, robot *testRobot) *http.Client {
	t.Helper()
	var pc wire.PairCodeBody
	robot.expect(wire.KindPairCode, &pc)
	browser := newBrowser()
	resp, err := browser.Get(pc.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return browser
}

func postBody(t *testing.T, c *http.Client, ts *httptest.Server, id, body string) int {
	t.Helper()
	resp, err := c.Post(ts.URL+"/api/robots/"+id+"/command", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestPingAndRobotEvents(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "chan-1", wire.ClassRobot)
	a := pairBrowser(t, robot)
	b := pairBrowser(t, robot) // the used code was replaced, so b scans a fresh one
	eventsA := openEvents(t, a, ts)
	eventsB := openEvents(t, b, ts)

	if code := postBody(t, a, ts, "chan-1", `{"command":"ping"}`); code != http.StatusBadRequest {
		t.Fatalf("ping without id: status %d, want 400", code)
	}
	if code := postBody(t, a, ts, "chan-1", `{"command":"ping","args":{"id":"abc123"}}`); code != http.StatusAccepted {
		t.Fatalf("ping: status %d, want 202", code)
	}
	var cmd wire.RobotCommandBody
	robot.expect(wire.KindRobotCommand, &cmd)
	if cmd.Command != "ping" || cmd.Args["id"] != "abc123" {
		t.Fatalf("robot got %+v", cmd)
	}
	robot.send(wire.KindRobotPong, wire.RobotPongBody{ID: "abc123", QueueMs: 1.5})

	pong := waitForEvent(t, eventsA, "pong")
	if pong["id"] != "abc123" || pong["robot_queue_ms"] != 1.5 || pong["server_robot_ms"].(float64) < 0 {
		t.Fatalf("pong event %+v", pong)
	}

	// Robot events reach every paired browser; b never sees a's pong.
	robot.send(wire.KindRobotEvent, wire.RobotEventBody{Name: "shake"})
	for _, events := range []<-chan sseMsg{eventsA, eventsB} {
		if ev := waitForEvent(t, events, "robot_event"); ev["name"] != "shake" || ev["robot"] != "chan-1" {
			t.Fatalf("robot event %+v", ev)
		}
	}
	for {
		select {
		case m := <-eventsB:
			if m.name == "pong" {
				t.Fatal("pong leaked to a browser that did not send the ping")
			}
			continue
		default:
		}
		break
	}
}

func (r *testRobot) expectBinary() []byte {
	r.t.Helper()
	r.ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		msgType, data, err := r.ws.ReadMessage()
		if err != nil {
			r.t.Fatalf("waiting for a binary message: %v", err)
		}
		if msgType == websocket.BinaryMessage {
			return data
		}
	}
}

func dialMedia(ts *httptest.Server, browser *http.Client, robotID, query, origin string) (*websocket.Conn, *http.Response, error) {
	d := websocket.Dialer{Jar: browser.Jar}
	h := http.Header{}
	h.Set("Origin", origin)
	return d.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/robots/"+robotID+"/media?"+query, h)
}

func TestMediaRelayPictureAndTap(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "chan-1", wire.ClassRobot)
	browser := pairBrowser(t, robot)

	// A page from another origin can't open the media socket with our cookie.
	if _, resp, err := dialMedia(ts, browser, "chan-1", "video=1", "http://evil.example"); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: err=%v resp=%v", err, resp)
	}
	// Nor can a browser that isn't paired.
	if _, resp, err := dialMedia(ts, newBrowser(), "chan-1", "video=1", ts.URL); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unpaired: err=%v resp=%v", err, resp)
	}

	media, _, err := dialMedia(ts, browser, "chan-1", "video=1", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	var cmd wire.RobotCommandBody
	robot.expect(wire.KindRobotCommand, &cmd)
	if cmd.Command != "camera" || cmd.Args["on"] != true {
		t.Fatalf("robot got %+v, want camera on", cmd)
	}

	// Video reaches the browser; audio does not (not subscribed).
	send := func(msg []byte) {
		if err := robot.ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			t.Fatal(err)
		}
	}
	send([]byte{wire.BinCameraJPEG, 0xFF, 0xD8, 1})
	send([]byte{wire.BinAudioPCM, 1, 2, 3, 4})
	send([]byte{wire.BinCameraJPEG, 0xFF, 0xD8, 2})
	media.SetReadDeadline(time.Now().Add(3 * time.Second))
	for _, want := range [][]byte{{wire.BinCameraJPEG, 0xFF, 0xD8, 1}, {wire.BinCameraJPEG, 0xFF, 0xD8, 2}} {
		_, got, err := media.ReadMessage()
		if err != nil || string(got) != string(want) {
			t.Fatalf("media got %v (%v), want %v", got, err, want)
		}
	}

	// The last viewer leaving turns the camera off.
	media.Close()
	robot.expect(wire.KindRobotCommand, &cmd)
	if cmd.Command != "camera" || cmd.Args["on"] != false {
		t.Fatalf("robot got %+v, want camera off", cmd)
	}

	// Pictures travel to the robot as binary BinShowJPEG.
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 9, 9}
	resp, err := browser.Post(ts.URL+"/api/robots/chan-1/picture", "image/jpeg", strings.NewReader(string(jpeg)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("picture: status %d", resp.StatusCode)
	}
	if got := robot.expectBinary(); string(got) != string(append([]byte{wire.BinShowJPEG}, jpeg...)) {
		t.Fatalf("robot got picture %v", got)
	}

	// Screen taps carry their coordinates.
	events := openEvents(t, browser, ts)
	robot.send(wire.KindRobotEvent, wire.RobotEventBody{Name: "screen_tap", Data: map[string]any{"x": 10, "y": 20}})
	ev := waitForEvent(t, events, "robot_event")
	if data, _ := ev["data"].(map[string]any); ev["name"] != "screen_tap" || data["x"] != 10.0 || data["y"] != 20.0 {
		t.Fatalf("tap event %+v", ev)
	}
}

func TestRobotEventsReplayedToLateBrowsers(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "chan-1", wire.ClassRobot)
	browser := pairBrowser(t, robot)

	// Both events happen while no event stream is open (e.g. the phone slept).
	robot.send(wire.KindRobotEvent, wire.RobotEventBody{Name: "screensaver_on", Data: map[string]any{"manual": 1}})
	robot.send(wire.KindRobotEvent, wire.RobotEventBody{Name: "screensaver_off"})
	time.Sleep(200 * time.Millisecond) // let the server record them

	events := openEvents(t, browser, ts)
	first := waitForEvent(t, events, "robot_event")
	second := waitForEvent(t, events, "robot_event")
	if first["name"] != "screensaver_on" || second["name"] != "screensaver_off" {
		t.Fatalf("replayed %v then %v", first["name"], second["name"])
	}
	if first["seq"].(float64) >= second["seq"].(float64) {
		t.Fatalf("seq not increasing: %v, %v", first["seq"], second["seq"])
	}
	if data, _ := first["data"].(map[string]any); data["manual"] != 1.0 {
		t.Fatalf("event data lost: %+v", first)
	}
}

func TestStandbyShownWhileOffline(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "chan-1", wire.ClassRobot)
	browser := pairBrowser(t, robot)

	robot.send(wire.KindRobotEvent, wire.RobotEventBody{Name: "standby", Data: map[string]any{"minutes": 5}})
	time.Sleep(100 * time.Millisecond)
	robot.ws.Close()
	time.Sleep(200 * time.Millisecond)

	views := listRobots(t, browser, ts)
	if len(views) != 1 || views[0].Online || views[0].StandbyUntil == nil {
		t.Fatalf("after standby: %+v", views)
	}
	if d := time.Until(*views[0].StandbyUntil); d < 4*time.Minute || d > 5*time.Minute {
		t.Fatalf("standby_until in %v, want about 5 min", d)
	}

	// Reconnecting ends it.
	back := connectRobot(t, ts, "chan-1", wire.ClassRobot)
	var accepted struct{}
	back.expect(wire.KindAccepted, &accepted)
	if views := listRobots(t, browser, ts); !views[0].Online || views[0].StandbyUntil != nil {
		t.Fatalf("after reconnect: %+v", views[0])
	}
}

func TestSpeakerAudioToRobot(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "chan-1", wire.ClassRobot)
	browser := pairBrowser(t, robot)

	media, _, err := dialMedia(ts, browser, "chan-1", "video=0&audio=0", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	pcm := []byte{wire.BinSpeakerPCM, 0xC0, 0x5D, 1, 0, 2, 0} // 24000 Hz, two samples
	if err := media.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
		t.Fatal(err)
	}
	if got := robot.expectBinary(); string(got) != string(pcm) {
		t.Fatalf("robot got %v, want %v", got, pcm)
	}
	// Other binary types from the browser are ignored.
	media.WriteMessage(websocket.BinaryMessage, []byte{wire.BinShowJPEG, 0xFF, 0xD8, 0})
	media.WriteMessage(websocket.BinaryMessage, pcm)
	if got := robot.expectBinary(); got[0] != wire.BinSpeakerPCM {
		t.Fatalf("robot got type %#x, want speaker audio", got[0])
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	stateFile := t.TempDir() + "/state.json"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	s1 := New(Config{RobotToken: testToken, PairTTL: time.Minute, StateFile: stateFile, Log: quiet})
	ts1 := httptest.NewServer(s1.Handler())
	s1.cfg.PublicURL = ts1.URL
	robot := connectRobot(t, ts1, "chan-1", wire.ClassRobot)
	browser := pairBrowser(t, robot)
	robot.send(wire.KindRobotEvent, wire.RobotEventBody{Name: "nfc_tag", Data: map[string]any{"uid": "04:A0"}})
	time.Sleep(200 * time.Millisecond) // let the server record it
	if err := s1.SaveState(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(stateFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v, %v", fi, err)
	}
	robot.ws.Close() // Close waits for the robot's handler to return
	ts1.Close()

	// A new server on the same file: the browser's cookie still pairs it with the robot.
	s2 := New(Config{RobotToken: testToken, PairTTL: time.Minute, StateFile: stateFile, Log: quiet})
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	defer ts2.CloseClientConnections() // ends the event stream, or Close would wait for it
	resp, err := browser.Get(ts2.URL + "/api/robots")
	if err != nil {
		t.Fatal(err)
	}
	var robots []robotView
	json.NewDecoder(resp.Body).Decode(&robots)
	resp.Body.Close()
	if len(robots) != 1 || robots[0].ID != "chan-1" || robots[0].Online || len(robots[0].Commands) == 0 {
		t.Fatalf("robots after restart: %+v", robots)
	}
	ev := waitForEvent(t, openEvents(t, browser, ts2), "robot_event")
	if data, _ := ev["data"].(map[string]any); ev["name"] != "nfc_tag" || data["uid"] != "04:A0" {
		t.Fatalf("event after restart: %+v", ev)
	}
}
