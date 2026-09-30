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

const (
	maxPictureBytes  = 192 << 10
	mediaStatsPeriod = 10 * time.Second
)

// mediaSub is one browser media WebSocket.
type mediaSub struct {
	robot   string
	session string
	video   bool
	audio   bool
	imu     bool
	touch   bool
	out     chan []byte
	sent    int // written by handleMedia
	dropped int // under Server.mu
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
	if kind == wire.BinSnapshot {
		s.storeSnapshot(robotID, msg[1:])
		return
	}
	stat := map[byte]int{wire.BinCameraJPEG: 0, wire.BinAudioPCM: 1, wire.BinAudioMulti: 1, wire.BinIMU: 2, wire.BinTouch: 3}
	i, known := stat[kind]
	if !known {
		return // only media types go to browsers
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.robots[robotID]; st != nil {
		st.mediaFrames[i]++
		st.mediaBytes[i] += len(msg)
	}
	for sub := range s.media {
		wants := [4]bool{sub.video, sub.audio, sub.imu, sub.touch}[i]
		if sub.robot != robotID || !wants {
			continue
		}
		select {
		case sub.out <- msg:
		default: // slow browser: drop this frame rather than delay the next ones
			sub.dropped++
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
	var wantVideo, wantAudio, wantIMU, wantTouch bool
	for sub := range s.media {
		if sub.robot == robotID {
			wantVideo = wantVideo || sub.video
			wantAudio = wantAudio || sub.audio
			wantIMU = wantIMU || sub.imu
			wantTouch = wantTouch || sub.touch
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
	if wantIMU != st.imuOn && slices.Contains(st.caps.Commands, "imu_stream") {
		st.imuOn = wantIMU
		send("imu_stream", wantIMU)
	}
	if wantTouch != st.touchOn && slices.Contains(st.caps.Commands, "touch_stream") {
		st.touchOn = wantTouch
		send("touch_stream", wantTouch)
	}
}

// handleMedia streams a paired robot's camera (?video=1) and microphone
// (?audio=1) to the browser as binary WebSocket messages.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	q := r.URL.Query()
	sub := &mediaSub{robot: id, session: session, video: q.Get("video") == "1", audio: q.Get("audio") == "1", imu: q.Get("imu") == "1", touch: q.Get("touch") == "1",
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
		s.mu.Lock()
		dropped := sub.dropped
		s.mu.Unlock()
		s.log.Info("media viewer left", "robot", id, "sent", sub.sent, "dropped", dropped)
	}()

	// The browser may send speaker audio (BinSpeakerPCM); reading also detects
	// that it went away.
	ws.SetReadLimit(maxSpeakerMessage)
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		logged := false
		for {
			kind, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage || len(msg) < 4 || msg[0] != wire.BinSpeakerPCM {
				continue
			}
			if s.forwardSpeaker(id, msg) && !logged {
				logged = true
				s.log.Info("speaker audio from browser", "robot", id)
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
				s.log.Info("media viewer write failed", "robot", id, "err", err)
				return
			}
			sub.sent++
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
	s.commandSent(id, "picture", map[string]any{"bytes": len(body)})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

// maxSpeakerMessage bounds one browser audio message (100 ms at 24 kHz is ~4.8 KB).
const maxSpeakerMessage = 64 << 10

// forwardSpeaker sends browser audio to the robot's speaker, if it has one and
// is online. Audio is dropped when the robot's queue is full: late audio is
// worse than a gap.
func (s *Server) forwardSpeaker(robotID string, msg []byte) bool {
	s.mu.Lock()
	st := s.robots[robotID]
	var conn *robotConn
	if st != nil && slices.Contains(st.caps.Commands, "speaker") {
		conn = st.conn
	}
	s.mu.Unlock()
	return conn != nil && conn.enqueueBinary(msg)
}

// logMediaStats logs what the robot streamed during the last period while the
// camera or microphone is on, and warns when a stream is on but nothing came.
func (s *Server) logMediaStats(robotID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.robots[robotID]
	if st == nil {
		return
	}
	if st.mediaStatsAt.IsZero() { // first check: start the first period now
		st.mediaStatsAt = time.Now()
		return
	}
	elapsed := time.Since(st.mediaStatsAt)
	if elapsed < mediaStatsPeriod {
		return
	}
	frames, bytes := st.mediaFrames, st.mediaBytes
	st.mediaFrames, st.mediaBytes, st.mediaStatsAt = [4]int{}, [4]int{}, time.Now()
	if !st.cameraOn && !st.micOn && !st.imuOn && !st.touchOn && frames == [4]int{} {
		return
	}
	secs := elapsed.Seconds()
	attrs := []any{"robot", robotID, "camera", st.cameraOn, "mic", st.micOn,
		"video_fps", round1(float64(frames[0]) / secs), "video_kbps", round1(float64(bytes[0]) / 1024 / secs),
		"audio_msgs", frames[1], "audio_kbps", round1(float64(bytes[1]) / 1024 / secs),
		"imu", st.imuOn, "imu_msgs", frames[2], "touch", st.touchOn, "touch_msgs", frames[3]}
	if (st.cameraOn && frames[0] == 0) || (st.micOn && frames[1] == 0) || (st.imuOn && frames[2] == 0) {
		s.log.Warn("media stalled: robot sends nothing for a stream that is on", attrs...)
		return
	}
	s.log.Info("media from robot", attrs...)
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// storeSnapshot keeps a robot's latest full-resolution still and tells its paired browsers.
func (s *Server) storeSnapshot(robotID string, jpeg []byte) {
	if !bytes.HasPrefix(jpeg, []byte{0xFF, 0xD8}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.robots[robotID]
	if st == nil {
		return
	}
	st.snapshot = bytes.Clone(jpeg)
	st.snapshotAt = time.Now()
	s.publish(robotID, "", sseEvent{name: "snapshot", data: mustJSON(map[string]any{
		"robot": robotID, "bytes": len(jpeg), "ts": st.snapshotAt,
	})})
	s.log.Info("snapshot", "robot", robotID, "bytes", len(jpeg))
}

// handleSnapshot serves a paired robot's latest full-resolution still.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	s.mu.Lock()
	paired := s.sessions[session][id]
	var jpeg []byte
	if st := s.robots[id]; st != nil {
		jpeg = st.snapshot
	}
	s.mu.Unlock()
	switch {
	case !paired:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
	case jpeg == nil:
		http.Error(w, "no snapshot yet", http.StatusNotFound)
	default:
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(jpeg)
	}
}
