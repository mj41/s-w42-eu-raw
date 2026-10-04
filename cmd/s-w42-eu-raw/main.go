// Command s-w42-eu-raw relays between Stackchan robots and browsers.
//
// Robots connect to ws://<host>/api/devices/connect with a bearer token.
// Browsers open http://<host>/ and pair by scanning the robot's QR code.
// With -tls-listen, the same dashboard is also served over HTTPS (for the
// microphone, which browsers allow only on secure pages).
//
// Other people's robots: with -robot-tokens-file, each listed robot connects with its own
// invite token. `s-w42-eu-raw invite <robot id>` makes one.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
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

	"github.com/mj41/s-w42-eu-raw/internal/server"
	"github.com/mj41/s-w42-eu-raw/wire"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "invite" {
		os.Exit(invite(os.Args[2:]))
	}
	var (
		listen         = flag.String("listen", ":8765", "HTTP listen address for robots and browsers")
		publicURL      = flag.String("public-url", "", "base URL browsers use to reach this server (default: http://<LAN IP>:<port>)")
		tokenFile      = flag.String("token-file", defaultTokenFile(), "file with the robot bearer token; generated if missing")
		pairTTL        = flag.Duration("pair-ttl", 5*time.Minute, "lifetime of a pairing code")
		debug          = flag.Bool("debug", false, "debug logging")
		stateFile      = flag.String("state-file", defaultStateFile(), "JSON file that keeps pairings and robots across restarts (\"\" disables)")
		uiDir          = flag.String("ui-dir", "", "development: serve index.html from this directory on every request (e.g. internal/server/ui), so UI edits need only a page reload")
		tlsListen      = flag.String("tls-listen", "", "also serve browsers over HTTPS on this address, e.g. :8766 (robots stay on -listen)")
		tlsCert        = flag.String("tls-cert", defaultConfigFile("tls-cert.pem"), "TLS certificate for -tls-listen; a self-signed one is created if missing")
		tlsKey         = flag.String("tls-key", defaultConfigFile("tls-key.pem"), "TLS key for -tls-listen")
		invites        = flag.String("robot-tokens-file", "", "per-robot invite tokens: lines \"<robot id> <sha256 of its token>\", read again when changed (\"\" disables; see `s-w42-eu-raw invite`)")
		proxies        = flag.Int("trusted-proxies", 0, "reverse proxies in front of this server that append the client address to X-Forwarded-For (1 behind one gateway); 0 ignores the header, which clients can forge")
		noAddrLim      = flag.Bool("no-address-limits", false, "turn off the per-address limits (failed logins, wrong pairing codes): for a server that cannot see client addresses, e.g. behind a TCP load balancer without the PROXY protocol, where every client would share one address")
		oidcIssuer     = flag.String("oidc-issuer", "", "sign-in: OpenID Connect issuer, e.g. https://auth.w42.eu (\"\" disables sign-in and self-service robot invites)")
		oidcClientID   = flag.String("oidc-client-id", "", "sign-in: this server's client id at the issuer")
		oidcSecretFile = flag.String("oidc-client-secret-file", "", "sign-in: file with the client secret")
		oidcRedirect   = flag.String("oidc-redirect-url", "", "sign-in: callback URL registered at the issuer (default <public-url>/auth/callback)")
		adminEmails    = flag.String("admin-emails", "", "comma-separated verified e-mails with no robot limit, who may remove any added robot")
		robotsPerAcct  = flag.Int("robots-per-account", 3, "how many robots one signed-in account (tier 4) may add")
		tiersFile      = flag.String("tiers-file", "", "tiers 1-3 by e-mail or GitHub login, one \"<tier> <email:|github:|github-id:>who\" per line, re-read when it changes (with sign-in only)")
		sponsorURL     = flag.String("sponsor-url", "", "where signed-in people are pointed when they hit a limit")
		firmwareDir    = flag.String("firmware-dir", "", "the official Embody Mode firmware (manifest.json and its parts) for the setup page /setup, from this directory instead of GitHub")
		firmwareRel    = flag.String("firmware-release", "latest", "else the official firmware for /setup from this GitHub release of mj41/StackChan: latest, a tag (embody-v…), or \"\" for none (/setup then only sets up robots that already have it)")
		offers         offerFlags
	)
	flag.Var(&offers, "offer", "offer robots another server: name=wss://host[,tokenfile] (repeatable; the token file holds that server's robot token)")
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

	var httpsPort string
	var cert tls.Certificate
	if *tlsListen != "" {
		if _, httpsPort, err = net.SplitHostPort(*tlsListen); err != nil {
			log.Error("tls-listen", "err", err)
			os.Exit(1)
		}
		if cert, err = loadOrCreateCert(*tlsCert, *tlsKey, log); err != nil {
			log.Error("TLS certificate", "err", err)
			os.Exit(1)
		}
	}

	srv := server.New(server.Config{
		RobotToken:       token,
		PublicURL:        strings.TrimRight(*publicURL, "/"),
		PairTTL:          *pairTTL,
		UIDir:            *uiDir,
		StateFile:        *stateFile,
		RobotTokensFile:  *invites,
		TrustedProxies:   *proxies,
		NoAddressLimits:  *noAddrLim,
		OIDCIssuer:       *oidcIssuer,
		OIDCClientID:     *oidcClientID,
		OIDCClientSecret: readSecretFile(*oidcSecretFile, log),
		OIDCRedirectURL:  *oidcRedirect,
		AdminEmails:      splitList(*adminEmails),
		RobotsPerAccount: *robotsPerAcct,
		TiersFile:        *tiersFile,
		SponsorURL:       *sponsorURL,
		FirmwareDir:      *firmwareDir,
		FirmwareRelease:  *firmwareRel,
		FirmwareCacheDir: firmwareCache(),
		HTTPSPort:        httpsPort,
		Offers:           offers,
		Log:              log,
	})
	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	var httpsSrv *http.Server
	if *tlsListen != "" {
		httpsSrv = &http.Server{Addr: *tlsListen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
		go func() {
			log.Info("serving browsers over HTTPS", "listen", *tlsListen, "cert", *tlsCert)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("https server", "err", err)
				os.Exit(1)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.RunStateSaver(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if httpsSrv != nil {
			httpsSrv.Shutdown(shutdownCtx)
		}
		httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("s-w42-eu-raw listening", "listen", *listen, "public_url", *publicURL, "token_file", *tokenFile, "state_file", *stateFile)
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
	return defaultConfigFile("robot-token")
}

// defaultConfigFile is name in ~/.config/stackchan-server (or the platform's config dir).
func defaultConfigFile(name string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return name
	}
	return filepath.Join(dir, "stackchan-server", name)
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

// offerFlags collects -offer name=url[,tokenfile] values.
type offerFlags []wire.OfferedServer

func (o *offerFlags) String() string { return fmt.Sprint(len(*o), " offers") }

func (o *offerFlags) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("want name=url[,tokenfile]")
	}
	url, tokenFile, _ := strings.Cut(rest, ",")
	if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		return fmt.Errorf("url must start with ws:// or wss://")
	}
	offer := wire.OfferedServer{Name: name, URL: url}
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return err
		}
		offer.Token = strings.TrimSpace(string(b))
	}
	*o = append(*o, offer)
	return nil
}

// invite prints a new invite token for one robot and the line for -robot-tokens-file.
func invite(args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: s-w42-eu-raw invite <robot id>   (e.g. stackchan-0a1b2c3d4e50)")
		return 2
	}
	token, line, err := server.NewRobotInvite(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "invite:", err)
		return 1
	}
	fmt.Printf("Token for the robot (its sdkconfig, CONFIG_STACKCHAN_EMBODY_TOKEN); give it only to its owner:\n  %s\n", token)
	fmt.Printf("Line for the server's -robot-tokens-file (holds only the hash):\n  %s\n", line)
	return 0
}

// readSecretFile reads a one-line secret; "" for no file. A missing file is fatal.
func readSecretFile(path string, log *slog.Logger) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Error("secret file", "file", path, "err", err)
		os.Exit(1)
	}
	return strings.TrimSpace(string(b))
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// firmwareCache: where firmware fetched from GitHub is kept (~/.cache/stackchan-server/firmware).
func firmwareCache() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "stackchan-server", "firmware")
}
