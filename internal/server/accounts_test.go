package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mj41/s-w42-eu-raw/wire"
)

// fakeIssuer is a minimal OpenID Connect provider: discovery, keys, and a token endpoint
// that checks PKCE and returns a signed ID token for codes the test hands out.
type fakeIssuer struct {
	t   *testing.T
	ts  *httptest.Server
	key *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]fakeCode
}

type fakeCode struct {
	nonce, challenge, sub, email, name string
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newFakeIssuer(t *testing.T) *fakeIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{t: t, key: key, codes: map[string]fakeCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.ts.URL, "authorization_endpoint": f.ts.URL + "/auth", "token_endpoint": f.ts.URL + "/token",
			"jwks_uri": f.ts.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		c, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || b64(sum[:]) != c.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		now := time.Now()
		claims := map[string]any{"iss": f.ts.URL, "sub": c.sub, "aud": "chan", "iat": now.Unix(),
			"exp": now.Add(time.Hour).Unix(), "nonce": c.nonce, "email": c.email, "email_verified": true, "name": c.name}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "token_type": "Bearer", "expires_in": 3600,
			"id_token": f.sign(claims)})
	})
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeIssuer) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	p, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

// user is one browser with its own cookies.
type user struct {
	t      *testing.T
	c      *http.Client
	server string
}

func newUser(t *testing.T, server string) *user {
	jar, _ := cookiejar.New(nil)
	return &user{t: t, server: server, c: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// signIn runs the whole login: /auth/login, the issuer (faked), /auth/callback.
func (u *user) signIn(f *fakeIssuer, sub, email, name string) {
	u.t.Helper()
	resp, err := u.c.Get(u.server + "/auth/login")
	if err != nil || resp.StatusCode != http.StatusFound {
		u.t.Fatalf("login: %v %v", err, resp)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), f.ts.URL+"/auth") || q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
		u.t.Fatalf("login redirect: %s", loc)
	}
	f.mu.Lock()
	f.codes["code-"+sub] = fakeCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), sub: sub, email: email, name: name}
	f.mu.Unlock()
	resp, err = u.c.Get(u.server + "/auth/callback?code=code-" + sub + "&state=" + url.QueryEscape(q.Get("state")))
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		u.t.Fatalf("callback: %v %d %s", err, resp.StatusCode, body)
	}
}

func (u *user) do(method, path, body string, sameOrigin bool) (int, string) {
	u.t.Helper()
	req, _ := http.NewRequest(method, u.server+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if sameOrigin {
		req.Header.Set("Origin", u.server)
	}
	resp, err := u.c.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// fakeManager answers robot-auth like a Stackchan manager, from a table the test controls.
type fakeManager struct {
	ts     *httptest.Server
	mu     sync.Mutex
	robots map[string]fakeManaged // robot id -> its token for this app and owner
	asked  int
}

type fakeManaged struct {
	token, owner string
	public       bool
}

const managerSecret = "app-secret"

func newFakeManager(t *testing.T) *fakeManager {
	m := &fakeManager{robots: map[string]fakeManaged{}}
	m.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/robot-auth" || r.Header.Get("Authorization") != "Bearer "+managerSecret {
			http.Error(w, "unknown app", http.StatusUnauthorized)
			return
		}
		var req struct{ Robot, Token string }
		json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.asked++
		rb, ok := m.robots[req.Robot]
		if !ok || rb.token != req.Token {
			json.NewEncoder(w).Encode(map[string]any{"ok": false})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "owner": rb.owner, "owner_name": "x", "public": rb.public})
	}))
	t.Cleanup(m.ts.Close)
	return m
}

// set gives a robot a token for this app, owned by owner (an account key: issuer|subject).
func (m *fakeManager) set(id, token, owner string, public bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.robots[id] = fakeManaged{token, owner, public}
}

func newSignInServer(t *testing.T, f *fakeIssuer, stateFile string) (*httptest.Server, *Server) {
	ts, s, _ := newManagedServer(t, f, stateFile)
	return ts, s
}

// newManagedServer is a server with sign-in and a (fake) manager.
func newManagedServer(t *testing.T, f *fakeIssuer, stateFile string) (*httptest.Server, *Server, *fakeManager) {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := newFakeManager(t)
	s := New(Config{RobotToken: testToken, PairTTL: time.Minute, Log: quiet, StateFile: stateFile,
		ManagerURL: m.ts.URL, ManagerSecret: managerSecret,
		OIDCIssuer: f.ts.URL, OIDCClientID: "chan", OIDCClientSecret: "secret",
		OIDCRedirectURL: "http://chan.example/auth/callback", AdminEmails: []string{"boss@example.com"},
		Offers: []wire.OfferedServer{{Name: "cloud", URL: "wss://chan.example", Token: "t0k"}}})
	ts := httptest.NewServer(s.Handler())
	s.cfg.PublicURL = "https://chan.example"
	t.Cleanup(ts.Close)
	return ts, s, m
}

// A robot set up by the manager connects with its token for this app; the manager says whose
// it is. Wrong tokens and unknown robots are refused; a confirmed robot keeps working for a
// while when the manager is down; a refusal is never cached.
func TestManagedRobot(t *testing.T) {
	f := newFakeIssuer(t)
	ts, _, m := newManagedServer(t, f, "")
	const id = "stackchan-0a1b2c3d4e50"
	if got := dialStatus(ts, "tok-1", id); got != http.StatusUnauthorized {
		t.Fatalf("unknown to the manager: %d", got)
	}
	m.set(id, "tok-1", f.ts.URL+"|ema", false)
	registerGuest(t, ts, "tok-1", id) // refused a moment ago, works now: refusals are not cached
	if got := dialStatus(ts, "tok-2", id); got != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %d", got)
	}
	ema := newUser(t, ts.URL)
	ema.signIn(f, "ema", "ema@example.com", "Ema")
	if got := ema.pairedIDs(); got != id {
		t.Fatalf("the owner (from the manager) has the robot: %q", got)
	}
	// The manager goes away: the robot it confirmed still connects (cached).
	m.ts.Close()
	registerGuest(t, ts, "tok-1", id)
}

func TestCallbackChecks(t *testing.T) {
	f := newFakeIssuer(t)
	ts, _ := newSignInServer(t, f, "")
	u := newUser(t, ts.URL)
	if code, _ := u.do("GET", "/auth/callback?code=x&state=unknown", "", false); code != http.StatusBadRequest {
		t.Fatalf("unknown state: %d", code)
	}
	// A state started in another browser does not sign this one in.
	other := newUser(t, ts.URL)
	resp, _ := other.c.Get(ts.URL + "/auth/login")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if code, _ := u.do("GET", "/auth/callback?code=x&state="+url.QueryEscape(loc.Query().Get("state")), "", false); code != http.StatusBadRequest {
		t.Fatalf("state of another browser: %d", code)
	}
}

func TestRobotURL(t *testing.T) {
	for _, c := range []struct {
		public, robot string
		reachable     bool
	}{
		{"https://chan.example/", "wss://chan.example", true},
		{"http://192.168.1.10:8765", "ws://192.168.1.10:8765", true},
		{"http://localhost:8765", "ws://localhost:8765", false},
		{"http://127.0.0.1:8799", "ws://127.0.0.1:8799", false},
		{"http://[::1]:8765", "ws://[::1]:8765", false},
		{"", "", false},
	} {
		s := &Server{cfg: Config{PublicURL: c.public}}
		if got := s.robotURL(); got != c.robot {
			t.Errorf("robotURL(%q) = %q, want %q", c.public, got, c.robot)
		}
		if got := s.robotURLReachable(); got != c.reachable {
			t.Errorf("robotURLReachable(%q) = %v, want %v", c.public, got, c.reachable)
		}
	}
}
