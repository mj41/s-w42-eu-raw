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
	"path/filepath"
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

func (u *user) addRobot(id string) (int, string) {
	code, body := u.do("POST", "/api/my/robots", `{"robot_id":"`+id+`"}`, true)
	var r struct{ Token string }
	json.Unmarshal([]byte(body), &r)
	return code, r.Token
}

func newSignInServer(t *testing.T, f *fakeIssuer, stateFile string) (*httptest.Server, *Server) {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{RobotToken: testToken, PairTTL: time.Minute, Log: quiet, StateFile: stateFile,
		OIDCIssuer: f.ts.URL, OIDCClientID: "chan", OIDCClientSecret: "secret",
		OIDCRedirectURL: "http://chan.example/auth/callback", AdminEmails: []string{"boss@example.com"},
		Offers: []wire.OfferedServer{{Name: "cloud", URL: "wss://chan.example", Token: "t0k"}}})
	ts := httptest.NewServer(s.Handler())
	s.cfg.PublicURL = "https://chan.example"
	t.Cleanup(ts.Close)
	return ts, s
}

func TestSignInAndAddRobot(t *testing.T) {
	f := newFakeIssuer(t)
	ts, _ := newSignInServer(t, f, "")
	ema := newUser(t, ts.URL)

	if _, body := ema.do("GET", "/api/me", "", false); !strings.Contains(body, `"signed_in":false`) || !strings.Contains(body, `"sign_in":true`) {
		t.Fatalf("me before sign-in: %s", body)
	}
	if code, _ := ema.addRobot("stackchan-0a1b2c3d4e50"); code != http.StatusUnauthorized {
		t.Fatalf("add without sign-in: %d", code)
	}
	ema.signIn(f, "ema", "ema@example.com", "Ema")
	if _, body := ema.do("GET", "/api/me", "", false); !strings.Contains(body, `"name":"Ema"`) || !strings.Contains(body, `"limit":3`) {
		t.Fatalf("me: %s", body)
	}

	// Add a robot: the token works for that robot, as a guest (no offers), and is shown once.
	code, token := ema.addRobot("stackchan-0a1b2c3d4e50")
	if code != http.StatusOK || len(token) != 64 {
		t.Fatalf("add: %d %q", code, token)
	}
	kinds := registerGuest(t, ts, token, "stackchan-0a1b2c3d4e50")
	if strings.Contains(strings.Join(kinds, " "), wire.KindServerOffer) {
		t.Fatalf("an added robot got the offers: %v", kinds)
	}
	if got := dialStatus(ts, token, "stackchan-0a1b2c3d4e51"); got != http.StatusUnauthorized {
		t.Fatalf("token for another id: %d", got)
	}
	if _, body := ema.do("GET", "/api/me", "", false); strings.Contains(body, token) {
		t.Fatal("/api/me must never return a token")
	}

	// CSRF: changing requests need this server's Origin.
	if code, _ := ema.do("POST", "/api/my/robots", `{"robot_id":"stackchan-0a1b2c3d4e52"}`, false); code != http.StatusForbidden {
		t.Fatalf("add without Origin: %d", code)
	}

	// Another account cannot claim it; the owner's robots cannot be claimed at all.
	jan := newUser(t, ts.URL)
	jan.signIn(f, "jan", "jan@example.com", "Jan")
	if code, _ := jan.addRobot("stackchan-0a1b2c3d4e50"); code != http.StatusConflict {
		t.Fatalf("other account claims: %d", code)
	}
	connectRobot(t, ts, "stackchan-owner00001", wire.ClassRobot)
	time.Sleep(50 * time.Millisecond)
	if code, _ := jan.addRobot("stackchan-owner00001"); code != http.StatusConflict {
		t.Fatalf("claim of the owner's robot: %d", code)
	}

	// A new token replaces the old one.
	code, token2 := ema.addRobot("stackchan-0a1b2c3d4e50")
	if code != http.StatusOK || token2 == token {
		t.Fatalf("new token: %d", code)
	}
	if got := dialStatus(ts, token, "stackchan-0a1b2c3d4e50"); got != http.StatusUnauthorized {
		t.Fatalf("old token after a new one: %d", got)
	}

	// The limit: 3 per account.
	for _, id := range []string{"stackchan-000000000002", "stackchan-000000000003"} {
		if code, _ := ema.addRobot(id); code != http.StatusOK {
			t.Fatalf("add %s: %d", id, code)
		}
	}
	if code, _ := ema.addRobot("stackchan-000000000004"); code != http.StatusForbidden {
		t.Fatalf("over the limit: %d", code)
	}

	// Removing revokes at once; only the owner (or an admin) may.
	if code, _ := jan.do("DELETE", "/api/my/robots/stackchan-0a1b2c3d4e50", "", true); code != http.StatusNotFound {
		t.Fatalf("someone else removes: %d", code)
	}
	if code, _ := ema.do("DELETE", "/api/my/robots/stackchan-0a1b2c3d4e50", "", true); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if got := dialStatus(ts, token2, "stackchan-0a1b2c3d4e50"); got != http.StatusUnauthorized {
		t.Fatalf("token after removal: %d", got)
	}

	// Sign out.
	if code, _ := ema.do("POST", "/auth/logout", "", true); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if _, body := ema.do("GET", "/api/me", "", false); !strings.Contains(body, `"signed_in":false`) {
		t.Fatalf("after logout: %s", body)
	}
}

func TestAdminHasNoLimit(t *testing.T) {
	f := newFakeIssuer(t)
	ts, _ := newSignInServer(t, f, "")
	boss := newUser(t, ts.URL)
	boss.signIn(f, "boss", "boss@example.com", "Boss")
	for i := 0; i < 5; i++ {
		if code, _ := boss.addRobot("stackchan-00000000000" + string(rune('a'+i))); code != http.StatusOK {
			t.Fatalf("admin add %d: %d", i, code)
		}
	}
	// The admin's own robot (seen with the shared token) can be added to the admin's account:
	// the setup page does that; then it connects with its own token.
	connectRobot(t, ts, "stackchan-owner00001", wire.ClassRobot)
	time.Sleep(50 * time.Millisecond)
	code, token := boss.addRobot("stackchan-owner00001")
	if code != http.StatusOK {
		t.Fatalf("admin adds their own robot: %d", code)
	}
	registerGuest(t, ts, token, "stackchan-owner00001")
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

func TestOwnedInvitesSurviveRestart(t *testing.T) {
	f := newFakeIssuer(t)
	path := filepath.Join(t.TempDir(), "state.json")
	ts, s := newSignInServer(t, f, path)
	u := newUser(t, ts.URL)
	u.signIn(f, "ema", "ema@example.com", "Ema")
	_, token := u.addRobot("stackchan-0a1b2c3d4e50")
	if err := s.SaveState(); err != nil {
		t.Fatal(err)
	}
	ts2, _ := newSignInServer(t, f, path)
	registerGuest(t, ts2, token, "stackchan-0a1b2c3d4e50")
}

func TestFleetPageServed(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := newBrowser().Get(ts.URL + "/robots")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "<title>Your robots") {
		t.Fatalf("GET /robots: %d %.80s", resp.StatusCode, b)
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
