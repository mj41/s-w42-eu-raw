package server

import (
	"time"

	"github.com/mj41/s-w42-eu-raw/robotauth"
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
