package server

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mj41/stackchan-server/internal/wire"
)

// Liveness timing, same as yolovm-pilot (from Rancher remotedialer).
const (
	pingPeriod      = 5 * time.Second
	pongWait        = 60 * time.Second
	writeWait       = 10 * time.Second
	registerTimeout = 10 * time.Second
	maxMessageBytes = 64 << 10
)

var robotIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Robots authenticate with a bearer token, not cookies, so a cross-origin
	// browser page gains nothing by opening this socket.
	CheckOrigin: func(r *http.Request) bool { return true },
}

type robotConn struct {
	id   string
	ws   *websocket.Conn
	send chan []byte

	closeOnce sync.Once
	done      chan struct{}
}

func (c *robotConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.ws.Close()
	})
}

// enqueue queues a frame for the writer; false if the robot is gone or stuck.
func (c *robotConn) enqueue(frame []byte) bool {
	select {
	case <-c.done:
		return false
	case c.send <- frame:
		return true
	default:
		return false
	}
}

func (s *Server) handleRobotConnect(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.tokenOK(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.Header.Get(wire.WorkerIDHeader)
	if !robotIDPattern.MatchString(id) {
		http.Error(w, "missing or invalid "+wire.WorkerIDHeader, http.StatusBadRequest)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("robot upgrade failed", "robot", id, "err", err)
		return
	}
	ws.SetReadLimit(maxMessageBytes)
	c := &robotConn{id: id, ws: ws, send: make(chan []byte, 32), done: make(chan struct{})}
	defer c.close()

	reg, reason := readRegister(ws, id)
	if reason != "" {
		s.log.Warn("robot rejected", "robot", id, "reason", reason, "remote", r.RemoteAddr)
		if f, err := wire.Marshal(wire.KindRejected, wire.Meta{}, wire.RejectedBody{Reason: reason}); err == nil {
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			ws.WriteMessage(websocket.TextMessage, f)
		}
		return
	}

	s.attach(c, reg)
	defer s.detach(c)
	s.log.Info("robot connected", "robot", id, "model", reg.Capabilities.Model,
		"firmware", reg.Capabilities.Firmware, "remote", r.RemoteAddr)
	defer s.log.Info("robot disconnected", "robot", id)

	if f, err := wire.Marshal(wire.KindAccepted, wire.Meta{WorkerID: id, SessionID: newCode()}, nil); err == nil {
		c.enqueue(f)
	}
	s.sendPairCode(c)

	go s.writeLoop(c)
	s.readLoop(c)
}

// readRegister waits for the mandatory first Register frame.
func readRegister(ws *websocket.Conn, id string) (wire.RegisterBody, string) {
	var reg wire.RegisterBody
	ws.SetReadDeadline(time.Now().Add(registerTimeout))
	_, data, err := ws.ReadMessage()
	if err != nil {
		return reg, "no Register frame"
	}
	frames, err := wire.Parse(data)
	if err != nil || len(frames) == 0 || frames[0].Kind != wire.KindRegister {
		return reg, "first frame must be Register"
	}
	f := frames[0]
	if f.Meta.WorkerID != "" && f.Meta.WorkerID != id {
		return reg, "meta.worker_id does not match " + wire.WorkerIDHeader
	}
	if err := f.Decode(&reg); err != nil {
		return reg, "invalid Register body"
	}
	if reg.Class != wire.ClassRobot {
		return reg, "unknown worker class"
	}
	return reg, ""
}

func (s *Server) sendPairCode(c *robotConn) {
	body := s.issueCode(c.id)
	if f, err := wire.Marshal(wire.KindPairCode, wire.Meta{WorkerID: c.id}, body); err == nil {
		c.enqueue(f)
	}
}

// writeLoop owns all writes to the socket: queued frames, pings, and
// replacing the pairing code before it expires.
func (s *Server) writeLoop(c *robotConn) {
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	rotate := time.NewTicker(s.cfg.PairTTL)
	defer rotate.Stop()
	for {
		select {
		case <-c.done:
			return
		case f := <-c.send:
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, f); err != nil {
				c.close()
				return
			}
		case <-ping.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				c.close()
				return
			}
		case <-rotate.C:
			s.sendPairCode(c)
		}
	}
}

func (s *Server) readLoop(c *robotConn) {
	resetDeadline := func() { c.ws.SetReadDeadline(time.Now().Add(pongWait)) }
	resetDeadline()
	c.ws.SetPongHandler(func(string) error { resetDeadline(); return nil })
	c.ws.SetPingHandler(func(data string) error {
		resetDeadline()
		return c.ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeWait))
	})

	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		resetDeadline()
		frames, err := wire.Parse(data)
		if err != nil {
			s.log.Warn("bad frame from robot", "robot", c.id, "err", err)
			continue
		}
		for _, f := range frames {
			s.handleRobotFrame(c, f)
		}
	}
}

func (s *Server) handleRobotFrame(c *robotConn, f wire.Frame) {
	switch f.Kind {
	case wire.KindHeartbeat:
		s.touch(c.id)
	case wire.KindRobotTelemetry:
		var body wire.RobotTelemetryBody
		if err := f.Decode(&body); err != nil {
			s.log.Warn("bad telemetry", "robot", c.id, "err", err)
			return
		}
		s.setTelemetry(c.id, body.Measurements)
	default:
		// Unknown kinds are ignored for forward compatibility.
		s.log.Debug("ignoring frame", "robot", c.id, "kind", f.Kind)
	}
}
