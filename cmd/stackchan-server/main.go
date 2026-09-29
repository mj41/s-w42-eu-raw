// Command stackchan-server relays between Stack-chan robots and browsers.
//
// Robots connect to ws://<host>/api/workers/connect with a bearer token.
// Browsers open http://<host>/ and pair by scanning the robot's QR code.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mj41/stackchan-server/internal/server"
)

func main() {
	var (
		listen    = flag.String("listen", ":8765", "HTTP listen address for robots and browsers")
		publicURL = flag.String("public-url", "", "base URL browsers use to reach this server (default: http://<LAN IP>:<port>)")
		tokenFile = flag.String("token-file", defaultTokenFile(), "file with the robot bearer token; generated if missing")
		pairTTL   = flag.Duration("pair-ttl", 5*time.Minute, "lifetime of a pairing code")
		debug     = flag.Bool("debug", false, "debug logging")
		stateFile = flag.String("state-file", defaultStateFile(), "JSON file that keeps pairings and robots across restarts (\"\" disables)")
		uiDir     = flag.String("ui-dir", "", "development: serve index.html from this directory on every request (e.g. internal/server/ui), so UI edits need only a page reload")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	token, err := loadOrCreateToken(*tokenFile, log)
	if err != nil {
		log.Error("robot token", "err", err)
		os.Exit(1)
	}
	if *publicURL == "" {
		*publicURL, err = defaultPublicURL(*listen)
		if err != nil {
			log.Error("public URL", "err", err)
			os.Exit(1)
		}
	}

	srv := server.New(server.Config{
		RobotToken: token,
		PublicURL:  strings.TrimRight(*publicURL, "/"),
		PairTTL:    *pairTTL,
		UIDir:      *uiDir,
		StateFile:  *stateFile,
		Log:        log,
	})
	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.RunStateSaver(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("stackchan-server listening", "listen", *listen, "public_url", *publicURL, "token_file", *tokenFile, "state_file", *stateFile)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
	if err := srv.SaveState(); err != nil {
		log.Warn("state not saved", "file", *stateFile, "err", err)
	}
}

// defaultStateFile follows the XDG base directory spec: $XDG_STATE_HOME or ~/.local/state.
func defaultStateFile() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "stackchan-server", "state.json")
}

func defaultTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "robot-token"
	}
	return filepath.Join(dir, "stackchan-server", "robot-token")
}

// loadOrCreateToken reads the shared robot token, generating a random one on first run.
func loadOrCreateToken(path string, log *slog.Logger) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(b))
		if token == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return token, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Info("generated new robot token", "path", path)
	return token, nil
}

// defaultPublicURL builds http://<LAN IP>:<port> so a phone on the same
// network can open the QR link.
func defaultPublicURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("parse -listen %q: %w", listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = lanIP()
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// lanIP returns the address of the interface used for outbound traffic.
// Dialing UDP sends no packets; it only picks a route.
func lanIP() string {
	if c, err := net.Dial("udp4", "192.0.2.1:80"); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "127.0.0.1"
}
