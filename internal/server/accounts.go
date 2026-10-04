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
	"errors"
	"fmt"
	"net/http"
	"net/netip"
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
	// From Dex's federated_claims: the upstream provider (connector id, e.g. "github") and its
	// user id, and the login there (preferred_username); for tiers (tiers.go).
	Provider   string `json:"provider,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	Login      string `json:"login,omitempty"`
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
	session, nonce, verifier, next string
	expires                        time.Time
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
		Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email", "profile", "federated:id"}}
	o.verifier = p.Verifier(&oidc.Config{ClientID: o.clientID})
	return o.conf, o.verifier, nil
}

func (o *oidcLogin) begin(session, next string) (state string, p pendingLogin) {
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
	// Back to a page of this server only: a path, never "//host" or a URL.
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		next = "/#your-robots"
	}
	p = pendingLogin{session: session, nonce: randHex(16), verifier: oauth2.GenerateVerifier(), next: next, expires: now.Add(loginTTL)}
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
	return emailTrusted(a) && slices.ContainsFunc(s.cfg.AdminEmails, func(e string) bool { return strings.EqualFold(e, a.Email) })
}

// emailTrusted: the e-mail may grant something (admin, a tier). Through Dex only from
// providers that verify it themselves: GitHub and Google. Microsoft's multi-tenant sign-in
// lets any tenant's admin set a user's e-mail ("nOAuth"), so it does not count; neither does
// any connector added later until it is checked and listed here.
func emailTrusted(a Account) bool {
	switch a.Provider {
	case "", "github", "google": // "": not through Dex; email_verified was checked
		return a.Email != ""
	}
	return false
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
	state, p := s.oidc.begin(session, r.URL.Query().Get("next"))
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
	s.pairOwnedLocked(session, acct)
	s.mu.Unlock()
	s.requestSave()
	s.log.Info("signed in", "account", accountLogID(acct.Key))
	http.Redirect(w, r, p.next, http.StatusSeeOther)
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
		Federated         struct {
			ConnectorID string `json:"connector_id"`
			UserID      string `json:"user_id"`
		} `json:"federated_claims"`
	}
	if err := idt.Claims(&c); err != nil {
		return Account{}, err
	}
	a := Account{Key: idt.Issuer + "|" + idt.Subject, Provider: c.Federated.ConnectorID, ProviderID: c.Federated.UserID}
	if a.Provider != "" {
		a.Login = c.PreferredUsername // the login at that provider (Dex's GitHub connector: the GitHub login)
	}
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

// robotURL is the address robots connect to: the public URL with ws:// or wss://.
func (s *Server) robotURL() string {
	return strings.Replace(strings.Replace(strings.TrimRight(s.cfg.PublicURL, "/"), "https://", "wss://", 1), "http://", "ws://", 1)
}

// robotURLReachable tells whether a robot can use robotURL: not when the public URL names
// this computer only (localhost, 127.0.0.1, ::1), which on the robot is the robot itself.
func (s *Server) robotURLReachable() bool {
	u, err := url.Parse(s.cfg.PublicURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	if h := u.Hostname(); strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && ip.IsLoopback() {
		return false
	}
	return true
}

// GET /api/me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	me := map[string]any{"sign_in": s.oidc != nil, "manager_url": s.cfg.ManagerURL}
	a, ok := s.account(session)
	if ok {
		s.mu.Lock()
		s.pairOwnedLocked(session, a) // robots that connected since sign-in
		s.mu.Unlock()
		me["name"], me["email"], me["admin"] = a.Name, a.Email, s.isAdmin(a)
	}
	t := s.sessionTier(session, time.Now())
	me["signed_in"], me["tier"], me["tier_hint"], me["full_video"] = ok, t, s.tierHint(t), tierTable[t].FullVideo
	writeJSON(w, http.StatusOK, me)
}
