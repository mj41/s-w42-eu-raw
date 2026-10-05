// Package sso signs people in to an app through a Stackchan manager (s-w42-eu-manager, e.g.
// sm.w42.eu): one sign-in for every app, and signing out in one signs out of all.
//
// The app sends a browser without a session to LoginURL; the manager sends it back to the
// app's return address with ?code=… (or ?error=login_required, when silent and not signed in
// there). The app trades the code for the account and a handle (Exchange), keeps a session of its
// own, and asks about the handle now and then (Check); its sign-out calls Logout. The manager
// knows the app by the return address (its web address in the manager's catalog) and by the
// app's secret, the same as for robot tokens (package robotauth).
package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	timeout = 10 * time.Second
	grace   = time.Hour // a sign-in confirmed within it stays on while the manager cannot be reached
	maxKeys = 10000
)

// Account is the signed-in person, as the manager knows them. Key (issuer|subject) is stable
// and the same in every app; Email is there only when the provider verified it; Provider is
// the upstream sign-in (Dex's connector id: "github", "google").
type Account struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Provider   string `json:"provider,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	Login      string `json:"login,omitempty"`
}

// Answer is the manager's answer about a code or a handle.
type Answer struct {
	OK      bool     `json:"ok"`
	Handle  string   `json:"handle,omitempty"`
	Account *Account `json:"account,omitempty"`
	CacheS  int      `json:"cache_s,omitempty"`
}

type cached struct {
	ok            bool
	fresh, usable time.Time
}

// Client talks to one manager for one app. Safe for concurrent use.
type Client struct {
	url, secret string
	http        *http.Client

	mu    sync.Mutex
	cache map[string]cached // handle -> the last answer
}

// New is a client of the manager at managerURL (e.g. https://sm.w42.eu) for the app with this
// secret; nil when either is empty.
func New(managerURL, secret string) *Client {
	if managerURL == "" || secret == "" {
		return nil
	}
	return &Client{url: strings.TrimRight(managerURL, "/"), secret: secret,
		http: &http.Client{Timeout: timeout}, cache: map[string]cached{}}
}

// LoginURL is where to send a browser to sign in; it comes back to returnURL (a page of this
// app) with ?code= or ?error=. Silent: never ask the person, only use a sign-in they have.
func (c *Client) LoginURL(returnURL string, silent bool) string {
	q := url.Values{"return": {returnURL}}
	if silent {
		q.Set("silent", "1")
	}
	return c.url + "/sso?" + q.Encode()
}

// Exchange trades a code from the browser's return for the account and its handle.
func (c *Client) Exchange(ctx context.Context, code string) (Answer, error) {
	var a Answer
	if err := c.post(ctx, "token", map[string]string{"code": code}, &a); err != nil {
		return Answer{}, err
	}
	if a.OK && (a.Account == nil || a.Account.Key == "" || a.Handle == "") {
		return Answer{}, fmt.Errorf("manager: incomplete answer")
	}
	if a.OK {
		c.remember(a.Handle, true, a.CacheS)
	}
	return a, nil
}

// Check tells whether the sign-in behind handle is still on (answers are cached as the manager
// says). When the manager cannot be reached, a sign-in it confirmed within the last hour counts
// as on, and err says why it was not asked.
func (c *Client) Check(ctx context.Context, handle string) (bool, error) {
	now := time.Now()
	c.mu.Lock()
	prev, found := c.cache[handle]
	c.mu.Unlock()
	if found && now.Before(prev.fresh) {
		return prev.ok, nil
	}
	var a Answer
	if err := c.post(ctx, "check", map[string]string{"handle": handle}, &a); err != nil {
		return found && prev.ok && now.Before(prev.usable), err
	}
	c.remember(handle, a.OK, a.CacheS)
	return a.OK, nil
}

// Logout ends the sign-in at the manager, and so in every app.
func (c *Client) Logout(ctx context.Context, handle string) error {
	c.mu.Lock()
	delete(c.cache, handle)
	c.mu.Unlock()
	return c.post(ctx, "logout", map[string]string{"handle": handle}, nil)
}

func (c *Client) remember(handle string, ok bool, cacheS int) {
	now := time.Now()
	keep := time.Duration(cacheS) * time.Second
	if !ok {
		keep = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxKeys {
		c.cache = map[string]cached{}
	}
	usable := now.Add(grace)
	if !ok {
		usable = now
	}
	c.cache[handle] = cached{ok: ok, fresh: now.Add(keep), usable: usable}
}

func (c *Client) post(ctx context.Context, what string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/api/sso/"+what, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("manager: %s", resp.Status)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<10)).Decode(out); err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	return nil
}
