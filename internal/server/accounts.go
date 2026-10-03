package server

// Sign-in and self-service robot invites. With an OpenID Connect provider configured (Dex
// at auth.w42.eu: GitHub, Google), people sign in on the dashboard and add their own robots:
// the server makes a robot's invite token, shows it once, and keeps only its hash, bound to
// the account. Signing in gives no access to any robot: browsers still pair only by the
// code on the robot's own screen.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	loginTTL                = 10 * time.Minute // from /auth/login to /auth/callback
	maxPendingLogins        = 1000
	defaultRobotsPerAccount = 3
)

// Account is a signed-in person. Key is the provider's issuer and subject: stable and
// unique; Name and Email are for display (Email only when the provider verified it).
type Account struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// ownedInvite is a robot an account added: the SHA-256 of its token, never the token.
type ownedInvite struct {
	RobotID   string    `json:"robot_id"`
	Hash      string    `json:"sha256"`
	Owner     string    `json:"owner"` // Account.Key
	OwnerName string    `json:"owner_name"`
	Created   time.Time `json:"created"`
}

type pendingLogin struct {
	session, nonce, verifier string
	expires                  time.Time
}

// oidcLogin talks to the provider. The provider is looked up on first use, so the server
// starts even while the provider is down.
type oidcLogin struct {
	issuer, clientID, clientSecret, redirectURL string

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	conf     *oauth2.Config
	pending  map[string]pendingLogin // state -> login in progress
}

func newOIDCLogin(cfg Config) *oidcLogin {
	if cfg.OIDCIssuer == "" || cfg.OIDCClientID == "" {
		return nil
	}
	redirect := cfg.OIDCRedirectURL
	if redirect == "" {
		redirect = strings.TrimRight(cfg.PublicURL, "/") + "/auth/callback"
	}
	return &oidcLogin{issuer: cfg.OIDCIssuer, clientID: cfg.OIDCClientID, clientSecret: cfg.OIDCClientSecret,
		redirectURL: redirect, pending: map[string]pendingLogin{}}
}

func (o *oidcLogin) setup(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conf != nil {
		return o.conf, o.verifier, nil
	}
	p, err := oidc.NewProvider(ctx, o.issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("sign-in provider %s: %w", o.issuer, err)
	}
	o.conf = &oauth2.Config{ClientID: o.clientID, ClientSecret: o.clientSecret, RedirectURL: o.redirectURL,
		Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email", "profile"}}
	o.verifier = p.Verifier(&oidc.Config{ClientID: o.clientID})
	return o.conf, o.verifier, nil
}

func (o *oidcLogin) begin(session string) (state string, p pendingLogin) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	for k, v := range o.pending {
		if now.After(v.expires) {
			delete(o.pending, k)
		}
	}
	if len(o.pending) >= maxPendingLogins {
		o.pending = map[string]pendingLogin{} // flooded: start over rather than grow
	}
	state = randHex(16)
	p = pendingLogin{session: session, nonce: randHex(16), verifier: oauth2.GenerateVerifier(), expires: now.Add(loginTTL)}
	o.pending[state] = p
	return state, p
}

func (o *oidcLogin) finish(state string) (pendingLogin, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.pending[state]
	delete(o.pending, state)
	return p, ok && time.Now().Before(p.expires)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sameOrigin is the CSRF check for requests that change something: browsers send Origin
// on POST and DELETE, and it must be this server.
func sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && u.Host != "" && u.Host == r.Host
}

func (s *Server) account(session string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.logins[session]
	return a, ok
}

func (s *Server) isAdmin(a Account) bool {
	return a.Email != "" && slices.ContainsFunc(s.cfg.AdminEmails, func(e string) bool { return strings.EqualFold(e, a.Email) })
}

func (s *Server) robotsPerAccount() int {
	if s.cfg.RobotsPerAccount > 0 {
		return s.cfg.RobotsPerAccount
	}
	return defaultRobotsPerAccount
}

// ownedInviteOK reports whether token is the self-service invite token of robot id.
func (s *Server) ownedInviteOK(id, token string) bool {
	s.mu.Lock()
	inv, found := s.owned[id]
	s.mu.Unlock()
	want, err := hex.DecodeString(inv.Hash)
	if err != nil || len(want) != sha256.Size {
		want = make([]byte, sha256.Size) // compare anyway: timing must not tell which ids exist
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want) == 1 && found && token != ""
}

// GET /auth/login: off to the provider.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	session := s.session(w, r)
	conf, _, err := s.oidc.setup(r.Context())
	if err != nil {
		s.log.Warn("sign-in", "err", err)
		http.Error(w, "sign-in is not available right now, try again later", http.StatusServiceUnavailable)
		return
	}
	state, p := s.oidc.begin(session)
	http.Redirect(w, r, conf.AuthCodeURL(state, oidc.Nonce(p.nonce), oauth2.S256ChallengeOption(p.verifier)), http.StatusFound)
}

// GET /auth/callback: back from the provider with a code.
func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	session := s.session(w, r)
	q := r.URL.Query()
	p, ok := s.oidc.finish(q.Get("state"))
	if !ok || p.session != session {
		authPage(w, http.StatusBadRequest, "Sign-in expired", "Please start again from the dashboard.")
		return
	}
	if e := q.Get("error"); e != "" {
		authPage(w, http.StatusOK, "Not signed in", "The sign-in was cancelled.")
		return
	}
	acct, err := s.exchange(r.Context(), q.Get("code"), p)
	if err != nil {
		s.log.Warn("sign-in failed", "err", err)
		authPage(w, http.StatusBadGateway, "Sign-in failed", "Please try again.")
		return
	}
	s.mu.Lock()
	s.logins[session] = acct
	s.mu.Unlock()
	s.requestSave()
	s.log.Info("signed in", "account", accountLogID(acct.Key))
	http.Redirect(w, r, "/#your-robots", http.StatusSeeOther)
}

func (s *Server) exchange(ctx context.Context, code string, p pendingLogin) (Account, error) {
	conf, verifier, err := s.oidc.setup(ctx)
	if err != nil {
		return Account{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := conf.Exchange(ctx, code, oauth2.VerifierOption(p.verifier))
	if err != nil {
		return Account{}, fmt.Errorf("code exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Account{}, errors.New("no id_token")
	}
	idt, err := verifier.Verify(ctx, raw)
	if err != nil {
		return Account{}, fmt.Errorf("id_token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(p.nonce)) != 1 {
		return Account{}, errors.New("id_token: wrong nonce")
	}
	var c struct {
		Email             string `json:"email"`
		EmailVerified     bool   `json:"email_verified"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idt.Claims(&c); err != nil {
		return Account{}, err
	}
	a := Account{Key: idt.Issuer + "|" + idt.Subject}
	if c.EmailVerified {
		a.Email = c.Email
	}
	for _, n := range []string{c.Name, c.PreferredUsername, a.Email, "you"} {
		if n != "" {
			a.Name = n
			break
		}
	}
	return a, nil
}

// accountLogID identifies an account in logs without its e-mail or name.
func accountLogID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

func authPage(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>%s</title><body style="font-family:system-ui;padding:16px"><h1>%s</h1><p>%s</p>`+
		`<p><a href="/">Open dashboard</a></p>`, title, title, text)
}

// POST /auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	session := s.session(w, r)
	s.mu.Lock()
	delete(s.logins, session)
	s.mu.Unlock()
	s.requestSave()
	w.WriteHeader(http.StatusNoContent)
}

type myRobot struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Online  bool      `json:"online"`
}

// GET /api/me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	a, ok := s.account(s.session(w, r))
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"signed_in": false, "sign_in": s.oidc != nil})
		return
	}
	s.mu.Lock()
	robots := []myRobot{}
	for _, inv := range s.owned {
		if inv.Owner == a.Key {
			st := s.robots[inv.RobotID]
			robots = append(robots, myRobot{ID: inv.RobotID, Created: inv.Created, Online: st != nil && st.conn != nil})
		}
	}
	s.mu.Unlock()
	slices.SortFunc(robots, func(x, y myRobot) int { return strings.Compare(x.ID, y.ID) })
	writeJSON(w, http.StatusOK, map[string]any{
		"signed_in": true, "sign_in": true, "name": a.Name, "email": a.Email,
		"admin": s.isAdmin(a), "limit": s.robotsPerAccount(), "robots": robots,
	})
}

// POST /api/my/robots {"robot_id"}: add a robot, or a new token for one of your own.
func (s *Server) handleAddMyRobot(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	a, ok := s.account(s.session(w, r))
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	var req struct {
		RobotID string `json:"robot_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		http.Error(w, `body must be {"robot_id": "..."}`, http.StatusBadRequest)
		return
	}
	id := strings.ToLower(strings.TrimSpace(req.RobotID))
	token, line, err := NewRobotInvite(id)
	if err != nil {
		http.Error(w, "invalid robot id: it looks like stackchan-0a1b2c3d4e50", http.StatusBadRequest)
		return
	}
	hash := strings.Fields(line)[1]
	if s.invites != nil && s.invites.has(id) {
		http.Error(w, "this robot id is already invited on this server", http.StatusConflict)
		return
	}

	s.mu.Lock()
	if s.ownerRobots[id] {
		s.mu.Unlock()
		http.Error(w, "this robot id belongs to the server's owner", http.StatusConflict)
		return
	}
	old, exists := s.owned[id]
	if exists && old.Owner != a.Key {
		s.mu.Unlock()
		http.Error(w, "another account already added this robot", http.StatusConflict)
		return
	}
	if !exists && !s.isAdmin(a) {
		n := 0
		for _, inv := range s.owned {
			if inv.Owner == a.Key {
				n++
			}
		}
		if n >= s.robotsPerAccount() {
			s.mu.Unlock()
			http.Error(w, fmt.Sprintf("you can add up to %d robots", s.robotsPerAccount()), http.StatusForbidden)
			return
		}
	}
	s.owned[id] = ownedInvite{RobotID: id, Hash: hash, Owner: a.Key, OwnerName: a.Name, Created: time.Now()}
	var conn *robotConn
	if st := s.robots[id]; st != nil && exists {
		conn = st.conn // connected with the old token: it must use the new one
	}
	s.mu.Unlock()
	if conn != nil {
		conn.close()
	}
	s.requestSave()
	s.log.Info("robot added by account", "robot", id, "account", accountLogID(a.Key), "new_token", exists)

	serverURL := strings.Replace(strings.Replace(strings.TrimRight(s.cfg.PublicURL, "/"), "https://", "wss://", 1), "http://", "ws://", 1)
	writeJSON(w, http.StatusOK, map[string]any{
		"robot_id": id, "token": token, "server_url": serverURL,
		"sdkconfig": fmt.Sprintf("CONFIG_STACKCHAN_EMBODY_SERVER_URL=%q\nCONFIG_STACKCHAN_EMBODY_TOKEN=%q\n", serverURL, token),
	})
}

// DELETE /api/my/robots/{id}: revoke one of your robots (admins: any added robot).
func (s *Server) handleRemoveMyRobot(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	a, ok := s.account(s.session(w, r))
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	inv, exists := s.owned[id]
	if !exists || (inv.Owner != a.Key && !s.isAdmin(a)) {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	delete(s.owned, id)
	var conn *robotConn
	if st := s.robots[id]; st != nil {
		conn = st.conn
	}
	s.mu.Unlock()
	if conn != nil {
		conn.close()
	}
	s.requestSave()
	s.log.Info("robot removed by account", "robot", id, "account", accountLogID(a.Key))
	w.WriteHeader(http.StatusNoContent)
}
