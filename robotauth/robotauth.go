// Package robotauth checks robot tokens with a Stackchan manager (s-w42-eu-manager, e.g.
// sm.w42.eu), for the apps it sets robots up for (s-w42-eu-raw, s-w42-eu-pet, ...).
//
// A robot set up by the manager carries a token of its own for each app. When it connects, the
// app asks the manager (POST <manager>/api/robot-auth, authenticated with the app's secret) and
// learns the robot's owner and whether it is public. Answers are cached as long as the manager
// says; refusals are not cached; when the manager cannot be reached, a robot it confirmed within
// the last hour may still connect.
package robotauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	timeout = 10 * time.Second
	grace   = time.Hour // a confirmed robot, while the manager cannot be reached
	maxKeys = 10000
)

// Auth is the manager's answer about a robot and token.
type Auth struct {
	OK        bool   `json:"ok"`
	Owner     string `json:"owner"`
	OwnerName string `json:"owner_name"`
	Public    bool   `json:"public"`
	CacheS    int    `json:"cache_s"`
}

type cachedAuth struct {
	auth          Auth
	fresh, usable time.Time // ask again after fresh; use when the manager is down until usable
}

// Client asks one manager. Safe for concurrent use.
type Client struct {
	url, secret string
	http        *http.Client

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cachedAuth
}

// New is a client of the manager at url (e.g. https://sm.w42.eu) for the app with this secret;
// nil when either is empty (no manager).
func New(url, secret string) *Client {
	if url == "" || secret == "" {
		return nil
	}
	return &Client{url: strings.TrimRight(url, "/"), secret: secret,
		http: &http.Client{Timeout: timeout}, cache: map[[sha256.Size]byte]cachedAuth{}}
}

// Check asks the manager whether token is robot's token for this app (or answers from the
// cache). With an error, the answer may still be OK: a robot confirmed recently while the
// manager cannot be reached.
func (c *Client) Check(ctx context.Context, robot, token string) (Auth, error) {
	key := sha256.Sum256([]byte(robot + "\x00" + token))
	now := time.Now()
	c.mu.Lock()
	cached, found := c.cache[key]
	c.mu.Unlock()
	if found && now.Before(cached.fresh) {
		return cached.auth, nil
	}
	auth, err := c.ask(ctx, robot, token)
	if err != nil {
		if found && cached.auth.OK && now.Before(cached.usable) {
			return cached.auth, err // the manager is down: a robot it confirmed recently may connect
		}
		return Auth{}, err
	}
	keep := time.Duration(auth.CacheS) * time.Second
	if !auth.OK {
		keep = 0 // a refusal is not cached: a robot set up a moment ago works at once
	}
	c.mu.Lock()
	if len(c.cache) >= maxKeys {
		c.cache = map[[sha256.Size]byte]cachedAuth{}
	}
	c.cache[key] = cachedAuth{auth: auth, fresh: now.Add(keep), usable: now.Add(grace)}
	c.mu.Unlock()
	return auth, nil
}

func (c *Client) ask(ctx context.Context, robot, token string) (Auth, error) {
	body, _ := json.Marshal(map[string]string{"robot": robot, "token": token})
	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/api/robot-auth", bytes.NewReader(body))
	if err != nil {
		return Auth{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return Auth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Auth{}, fmt.Errorf("manager: %s", resp.Status)
	}
	var auth Auth
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<10)).Decode(&auth); err != nil {
		return Auth{}, fmt.Errorf("manager: %w", err)
	}
	return auth, nil
}
