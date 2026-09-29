package server

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mj41/stackchan-server/internal/wire"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.session(w, r)
	page, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(page)
}

// handlePair is the QR target. GET because a phone camera opens it directly;
// the one-time code itself is the proof that the user can see the robot.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	code := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(r.URL.Query().Get("code")))

	robotID, viewers, conn, ok := s.redeem(session, code)
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">`+
			`<title>Pairing failed</title><body style="font-family:system-ui;padding:16px">`+
			`<h1>Pairing failed</h1><p>This code is invalid, expired, or already used. `+
			`Scan the QR code on the robot again.</p><p><a href="/">Open dashboard</a></p>`)
		return
	}
	s.log.Info("browser paired", "robot", robotID, "viewers", viewers)

	// Tell the robot, and replace the used code so the QR on screen stays valid.
	if conn != nil {
		if f, err := wire.Marshal(wire.KindPaired, wire.Meta{WorkerID: robotID}, wire.PairedBody{Viewers: viewers}); err == nil {
			conn.enqueue(f)
		}
		s.sendPairCode(conn)
	}
	s.broadcast(robotID)
	http.Redirect(w, r, "/?paired="+url.QueryEscape(robotID), http.StatusSeeOther)
}

func (s *Server) handleListRobots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.pairedViews(s.session(w, r)))
}

// handleEvents streams robot state as SSE: one full robotView per "robot" event.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	sub := &subscriber{session: session, events: make(chan robotView, 16)}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
	}()

	send := func(v robotView) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: robot\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, v := range s.pairedViews(session) {
		if !send(v) {
			return
		}
	}
	fmt.Fprint(w, ": ready\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case v := <-sub.events:
			if !send(v) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")

	// Requiring JSON forces a CORS preflight for cross-site requests.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var cmd wire.RobotCommandBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&cmd); err != nil || cmd.Command == "" {
		http.Error(w, "body must be {\"command\": \"...\"}", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	paired := s.sessions[session][id]
	st := s.robots[id]
	var conn *robotConn
	var commands []string
	if st != nil {
		conn = st.conn
		commands = st.caps.Commands
	}
	s.mu.Unlock()

	switch {
	case !paired || st == nil:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	case !slices.Contains(commands, cmd.Command):
		http.Error(w, "robot does not support command "+cmd.Command, http.StatusBadRequest)
		return
	case conn == nil:
		http.Error(w, "robot is offline", http.StatusConflict)
		return
	}

	f, err := wire.Marshal(wire.KindRobotCommand, wire.Meta{WorkerID: id}, cmd)
	if err != nil || !conn.enqueue(f) {
		http.Error(w, "robot is not accepting commands", http.StatusServiceUnavailable)
		return
	}
	s.log.Info("command sent", "robot", id, "command", cmd.Command)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
