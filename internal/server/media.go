package server

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mj41/stackchan-server/internal/wire"
)

// Media: the robot streams camera frames and microphone audio as binary
// WebSocket messages (wire.BinCameraJPEG, wire.BinAudioPCM) only while at
// least one paired browser watches or listens. Browsers get the same
// messages over /api/robots/{id}/media.

const maxPictureBytes = 192 << 10

// mediaSub is one browser media WebSocket.
type mediaSub struct {
	robot   string
	session string
	video   bool
	audio   bool
	out     chan []byte
}

// Browsers send the session cookie with the WebSocket handshake, so only
// accept pages from this server's own origin (no cross-site WebSocket hijack).
var browserUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 16 << 10,
	CheckOrigin: func(r *http.Request) bool {
		u, err := url.Parse(r.Header.Get("Origin"))
		return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
	},
}

// relayMedia forwards a binary message from a robot to the browsers that want it.
func (s *Server) relayMedia(robotID string, msg []byte) {
	if len(msg) < 2 {
		return
	}
	kind := msg[0]
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.media {
		if sub.robot != robotID || (kind == wire.BinCameraJPEG && !sub.video) || (kind == wire.BinAudioPCM && !sub.audio) {
			continue
		}
		select {
		case sub.out <- msg:
		default: // slow browser: drop this frame rather than delay the next ones
		}
	}
}

// syncMedia turns the robot's camera and microphone on or off to match the
// current subscribers. Must be called with s.mu held.
func (s *Server) syncMedia(robotID string) {
	st := s.robots[robotID]
	if st == nil || st.conn == nil {
		return
	}
	var wantVideo, wantAudio bool
	for sub := range s.media {
		if sub.robot == robotID {
			wantVideo = wantVideo || sub.video
			wantAudio = wantAudio || sub.audio
		}
	}
	send := func(command string, on bool) {
		f, err := wire.Marshal(wire.KindRobotCommand, wire.Meta{WorkerID: robotID},
			wire.RobotCommandBody{Command: command, Args: map[string]any{"on": on}})
		if err == nil && st.conn.enqueue(f) {
			s.log.Info("media", "robot", robotID, command, on)
		}
	}
	if wantVideo != st.cameraOn && slices.Contains(st.caps.Commands, "camera") {
		st.cameraOn = wantVideo
		send("camera", wantVideo)
	}
	if wantAudio != st.micOn && slices.Contains(st.caps.Commands, "mic") {
		st.micOn = wantAudio
		send("mic", wantAudio)
	}
}

// handleMedia streams a paired robot's camera (?video=1) and microphone
// (?audio=1) to the browser as binary WebSocket messages.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	q := r.URL.Query()
	sub := &mediaSub{robot: id, session: session, video: q.Get("video") == "1", audio: q.Get("audio") == "1",
		out: make(chan []byte, 8)}

	s.mu.Lock()
	paired := s.sessions[session][id]
	s.mu.Unlock()
	if !paired {
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	}
	ws, err := browserUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already answered (e.g. 403 for a foreign origin)
	}
	defer ws.Close()

	s.mu.Lock()
	s.media[sub] = struct{}{}
	s.syncMedia(id)
	s.mu.Unlock()
	s.log.Info("media viewer joined", "robot", id, "video", sub.video, "audio", sub.audio)
	defer func() {
		s.mu.Lock()
		delete(s.media, sub)
		s.syncMedia(id)
		s.mu.Unlock()
		s.log.Info("media viewer left", "robot", id)
	}()

	// The browser sends nothing; reading only detects that it went away.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := ws.NextReader(); err != nil {
				return
			}
		}
	}()
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	for {
		select {
		case <-gone:
			return
		case msg := <-sub.out:
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return
			}
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

// handlePicture sends a JPEG (scaled to 320x240 by the browser) to a paired
// robot, which shows it instead of the face until the "face" command.
func (s *Server) handlePicture(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "image/jpeg" {
		http.Error(w, "Content-Type must be image/jpeg", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPictureBytes))
	if err != nil || !bytes.HasPrefix(body, []byte{0xFF, 0xD8}) {
		http.Error(w, "body must be a JPEG up to 192 KB", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	paired := s.sessions[session][id]
	st := s.robots[id]
	var conn *robotConn
	var supported bool
	if st != nil {
		conn = st.conn
		supported = slices.Contains(st.caps.Commands, "image")
	}
	s.mu.Unlock()
	switch {
	case !paired || st == nil:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	case !supported:
		http.Error(w, "robot does not show pictures", http.StatusBadRequest)
		return
	case conn == nil:
		http.Error(w, "robot is offline", http.StatusConflict)
		return
	}
	if !conn.enqueueBinary(append([]byte{wire.BinShowJPEG}, body...)) {
		http.Error(w, "robot is not accepting commands", http.StatusServiceUnavailable)
		return
	}
	s.log.Info("picture sent", "robot", id, "bytes", len(body))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}
