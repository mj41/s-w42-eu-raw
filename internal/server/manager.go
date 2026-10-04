package server

// Robots set up by a Stackchan manager (s-w42-eu-manager, e.g. sm.w42.eu) carry a token of
// their own for this app. The manager knows them: when such a robot connects, this server asks
// it (POST <manager>/api/robot-auth, authenticated with this app's secret) and learns the
// robot's owner and whether it is public. Answers are cached as long as the manager says; when
// the manager cannot be reached, a robot it confirmed within the last hour may still connect.

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
	managerTimeout = 10 * time.Second
	managerGrace   = time.Hour // a confirmed robot, while the manager cannot be reached
	maxManagerKeys = 10000
)

// managerAuth is the manager's answer about a robot and token.
type managerAuth struct {
	OK        bool   `json:"ok"`
	Owner     string `json:"owner"`
	OwnerName string `json:"owner_name"`
	Public    bool   `json:"public"`
	CacheS    int    `json:"cache_s"`
}

type cachedAuth struct {
	auth          managerAuth
	fresh, usable time.Time // ask again after fresh; use when the manager is down until usable
}

type managerClient struct {
	url, secret string
	http        *http.Client

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cachedAuth
}

func newManagerClient(url, secret string) *managerClient {
	if url == "" || secret == "" {
		return nil
	}
	return &managerClient{url: strings.TrimRight(url, "/"), secret: secret,
		http: &http.Client{Timeout: managerTimeout}, cache: map[[sha256.Size]byte]cachedAuth{}}
}

// check asks the manager whether token is robot's token for this app (or answers from the cache).
func (c *managerClient) check(ctx context.Context, robot, token string) (managerAuth, error) {
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
		return managerAuth{}, err
	}
	keep := time.Duration(auth.CacheS) * time.Second
	if !auth.OK {
		keep = 0 // a refusal is not cached: a robot set up a moment ago works at once
	}
	c.mu.Lock()
	if len(c.cache) >= maxManagerKeys {
		c.cache = map[[sha256.Size]byte]cachedAuth{}
	}
	c.cache[key] = cachedAuth{auth: auth, fresh: now.Add(keep), usable: now.Add(managerGrace)}
	c.mu.Unlock()
	return auth, nil
}

func (c *managerClient) ask(ctx context.Context, robot, token string) (managerAuth, error) {
	body, _ := json.Marshal(map[string]string{"robot": robot, "token": token})
	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/api/robot-auth", bytes.NewReader(body))
	if err != nil {
		return managerAuth{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return managerAuth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return managerAuth{}, fmt.Errorf("manager: %s", resp.Status)
	}
	var auth managerAuth
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<10)).Decode(&auth); err != nil {
		return managerAuth{}, fmt.Errorf("manager: %w", err)
	}
	return auth, nil
}

// managedRobotLocked records what the manager said about a robot: its owner (whose signed-in
// browsers get it), and whether it is public (a robot that became private loses the pairings of
// everyone else). Must hold s.mu.
func (s *Server) managedRobotLocked(id string, auth managerAuth) {
	s.owned[id] = ownedInvite{RobotID: id, Owner: auth.Owner, OwnerName: auth.OwnerName, Created: time.Now()}
	for session, acct := range s.logins { // the owner's signed-in browsers get it without a code
		if acct.Key == auth.Owner {
			s.pairOwnedLocked(session, acct)
		}
	}
	switch {
	case auth.Public && !s.public[id]:
		s.public[id] = true
	case !auth.Public && s.public[id]:
		delete(s.public, id)
		s.unpairOthersLocked(id)
	}
	s.requestSave()
}
