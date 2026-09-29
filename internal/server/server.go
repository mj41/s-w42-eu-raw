// Package server relays between Stack-chan robots (outbound WebSocket) and
// browsers (HTML + REST + SSE).
//
// Pairing: after a robot registers it gets a short-lived one-time code and
// shows <public-url>/pair?code=... as a QR. The browser that opens that URL
// gets the robot added to its session cookie. Only paired sessions can see
// the robot's telemetry or send it commands.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mj41/stackchan-server/internal/wire"
)

//go:embed ui/index.html
var uiFS embed.FS

// Config configures a Server.
type Config struct {
	RobotToken string        // shared bearer token robots must present
	PublicURL  string        // base URL browsers use, e.g. http://192.168.0.18:8765
	PairTTL    time.Duration // lifetime of a pairing code
	Log        *slog.Logger
}

// Server holds all state in memory. A restart forgets pairings, and robots
// simply show a new QR code.
type Server struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	robots   map[string]*robotState     // by robot id; kept after disconnect
	codes    map[string]pairCode        // one-time pairing codes
	sessions map[string]map[string]bool // browser session id -> paired robot ids
	subs     map[*subscriber]struct{}   // open SSE streams
}

type pairCode struct {
	robotID string
	expires time.Time
}

type robotState struct {
	id          string
	caps        wire.RobotCapabilities
	labels      map[string]string
	conn        *robotConn // nil while offline
	lastSeen    time.Time
	telemetry   map[string]float64
	telemetryAt time.Time
}

// robotView is the browser-facing JSON for one robot.
type robotView struct {
	ID          string             `json:"id"`
	Online      bool               `json:"online"`
	Model       string             `json:"model,omitempty"`
	Firmware    string             `json:"firmware,omitempty"`
	Commands    []string           `json:"commands"`
	Telemetry   map[string]float64 `json:"telemetry"`
	TelemetryAt *time.Time         `json:"telemetry_at,omitempty"`
	LastSeen    time.Time          `json:"last_seen"`
}

type subscriber struct {
	session string
	events  chan robotView
}

func New(cfg Config) *Server {
	if cfg.PairTTL <= 0 {
		cfg.PairTTL = 5 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Server{
		cfg:      cfg,
		log:      cfg.Log,
		robots:   map[string]*robotState{},
		codes:    map[string]pairCode{},
		sessions: map[string]map[string]bool{},
		subs:     map[*subscriber]struct{}{},
	}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.ConnectPath, s.handleRobotConnect)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /pair", s.handlePair)
	mux.HandleFunc("GET /api/robots", s.handleListRobots)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/robots/{id}/command", s.handleCommand)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

func (s *Server) tokenOK(token string) bool {
	return s.cfg.RobotToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.RobotToken)) == 1
}

/* ------------------------------- robot state ------------------------------ */

// attach marks a robot online with a new connection, replacing any older one.
func (s *Server) attach(c *robotConn, reg wire.RegisterBody) {
	s.mu.Lock()
	st := s.robots[c.id]
	if st == nil {
		st = &robotState{id: c.id, telemetry: map[string]float64{}}
		s.robots[c.id] = st
	}
	old := st.conn
	st.conn = c
	st.caps = reg.Capabilities
	st.labels = reg.Labels
	st.lastSeen = time.Now()
	s.mu.Unlock()

	if old != nil {
		s.log.Info("robot reconnected, closing previous connection", "robot", c.id)
		old.close()
	}
	s.broadcast(c.id)
}

// detach marks a robot offline unless a newer connection already replaced c.
func (s *Server) detach(c *robotConn) {
	s.mu.Lock()
	st := s.robots[c.id]
	if st == nil || st.conn != c {
		s.mu.Unlock()
		return
	}
	st.conn = nil
	for code, pc := range s.codes {
		if pc.robotID == c.id {
			delete(s.codes, code)
		}
	}
	s.mu.Unlock()
	s.broadcast(c.id)
}

func (s *Server) touch(id string) {
	s.mu.Lock()
	if st := s.robots[id]; st != nil {
		st.lastSeen = time.Now()
	}
	s.mu.Unlock()
}

func (s *Server) setTelemetry(id string, m map[string]float64) {
	s.mu.Lock()
	st := s.robots[id]
	if st != nil {
		for k, v := range m {
			st.telemetry[k] = v
		}
		st.telemetryAt = time.Now()
		st.lastSeen = st.telemetryAt
	}
	s.mu.Unlock()
	if st != nil {
		s.broadcast(id)
	}
}

// view must be called with s.mu held.
func (s *Server) view(st *robotState) robotView {
	v := robotView{
		ID:        st.id,
		Online:    st.conn != nil,
		Model:     st.caps.Model,
		Firmware:  st.caps.Firmware,
		Commands:  slices.Clone(st.caps.Commands),
		Telemetry: make(map[string]float64, len(st.telemetry)),
		LastSeen:  st.lastSeen,
	}
	if v.Commands == nil {
		v.Commands = []string{}
	}
	for k, val := range st.telemetry {
		v.Telemetry[k] = val
	}
	if !st.telemetryAt.IsZero() {
		t := st.telemetryAt
		v.TelemetryAt = &t
	}
	return v
}

// broadcast pushes the robot's current state to every SSE stream paired with it.
func (s *Server) broadcast(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.robots[id]
	if st == nil {
		return
	}
	v := s.view(st)
	for sub := range s.subs {
		if !s.sessions[sub.session][id] {
			continue
		}
		select {
		case sub.events <- v:
		default: // slow browser; it gets the next full-state update
		}
	}
}

/* --------------------------------- pairing -------------------------------- */

// Unambiguous characters only (no 0/O, 1/I), easy to read off a screen.
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

func newCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

// issueCode replaces the robot's pairing code and returns the frame body to send.
func (s *Server) issueCode(robotID string) wire.PairCodeBody {
	code := newCode()
	s.mu.Lock()
	for c, pc := range s.codes {
		if pc.robotID == robotID {
			delete(s.codes, c)
		}
	}
	s.codes[code] = pairCode{robotID: robotID, expires: time.Now().Add(s.cfg.PairTTL)}
	s.mu.Unlock()
	return wire.PairCodeBody{
		Code:       code,
		URL:        s.cfg.PublicURL + "/pair?code=" + code,
		ExpiresInS: int(s.cfg.PairTTL / time.Second),
	}
}

// redeem consumes a code and pairs the session with its robot.
func (s *Server) redeem(session, code string) (robotID string, viewers int, conn *robotConn, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pc, found := s.codes[code]
	if !found || time.Now().After(pc.expires) {
		delete(s.codes, code)
		return "", 0, nil, false
	}
	delete(s.codes, code)
	if s.sessions[session] == nil {
		s.sessions[session] = map[string]bool{}
	}
	s.sessions[session][pc.robotID] = true
	for _, paired := range s.sessions {
		if paired[pc.robotID] {
			viewers++
		}
	}
	var c *robotConn
	if st := s.robots[pc.robotID]; st != nil {
		c = st.conn
	}
	return pc.robotID, viewers, c, true
}

/* -------------------------------- sessions -------------------------------- */

const sessionCookie = "stackchan_session"

// session returns the browser's session id, setting a new cookie if needed.
// Ids are random 32-byte hex; after a server restart an old cookie stays valid
// but has no paired robots.
func (s *Server) session(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && validSessionID(c.Value) {
		return c.Value
	}
	b := make([]byte, 32)
	rand.Read(b)
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int((365 * 24 * time.Hour) / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	return id
}

func validSessionID(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// pairedViews returns the robots paired with a session, sorted by id.
func (s *Server) pairedViews(session string) []robotView {
	s.mu.Lock()
	defer s.mu.Unlock()
	views := []robotView{}
	for id := range s.sessions[session] {
		if st := s.robots[id]; st != nil {
			views = append(views, s.view(st))
		}
	}
	slices.SortFunc(views, func(a, b robotView) int { return strings.Compare(a.ID, b.ID) })
	return views
}
