package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/mj41/s-w42-eu-raw/robotauth"
	"github.com/mj41/s-w42-eu-raw/wire"
)

// Robots set up by a Stackchan manager carry a token of their own for this app, checked with the
// manager (robotauth); it also says whose robot it is and whether it is public.

// managedRobotLocked records what the manager said about a robot: its owner (whose signed-in
// browsers get it), and whether it is public (a robot that became private loses the pairings of
// everyone else). Must hold s.mu.
func (s *Server) managedRobotLocked(id string, auth robotauth.Auth) {
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

// relayManaged tells the manager the robot is here (Seen: online, firmware, its app list version)
// and passes the robot's app list, as its owner set it on the manager and the manager
// signed it, on to the robot (it checks the signature itself). Sent when the robot connects and
// when the version changes (RunManagedRelay); the manager's answers are cached for a minute.
func (s *Server) relayManaged(ctx context.Context, c *robotConn) {
	if s.manager == nil || c.mgrToken == "" {
		return
	}
	auth, err := s.manager.Seen(ctx, c.id, c.mgrToken, robotauth.Seen{Firmware: c.firmware, AppsVersion: c.appsVersion.Load()})
	if err != nil || !auth.OK || auth.Managed == nil {
		return
	}
	version := managedVersion(auth.Managed.Payload)
	s.mu.Lock()
	sent := c.managedSent
	if version > sent {
		c.managedSent = version
	}
	s.mu.Unlock()
	if version <= sent {
		return
	}
	if f, err := wire.Marshal(wire.KindManagedApps, wire.Meta{WorkerID: c.id}, wire.ManagedAppsBody{Payload: auth.Managed.Payload, Sig: auth.Managed.Sig}); err == nil {
		c.enqueue(f)
		s.log.Info("app list relayed", "robot", c.id, "version", version)
	}
}

// managedVersion reads the list's version (the robot checks the rest).
func managedVersion(payload string) int32 {
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return 0
	}
	var p struct {
		Version int32 `json:"version"`
	}
	json.Unmarshal(b, &p)
	return p.Version
}

// RunManagedRelay relays changed app lists to the connected robots every minute until ctx ends.
func (s *Server) RunManagedRelay(ctx context.Context) {
	if s.manager == nil {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			var conns []*robotConn
			for _, rs := range s.robots {
				if rs.conn != nil && rs.conn.mgrToken != "" {
					conns = append(conns, rs.conn)
				}
			}
			s.mu.Unlock()
			for _, c := range conns {
				s.relayManaged(ctx, c)
			}
		}
	}
}
