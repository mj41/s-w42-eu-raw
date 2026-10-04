package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mj41/s-w42-eu-raw/wire"
)

// robotWithCode connects a robot and returns it with its current pairing code.
func robotWithCode(t *testing.T, ts *httptest.Server, token, id string) (*testRobot, string) {
	t.Helper()
	ws, _, err := dialRobot(ts, token, id)
	if err != nil {
		t.Fatalf("dial %s: %v", id, err)
	}
	t.Cleanup(func() { ws.Close() })
	r := &testRobot{t: t, ws: ws}
	r.send(wire.KindRegister, wire.RegisterBody{Class: wire.ClassRobot,
		Capabilities: wire.RobotCapabilities{Model: "test", Commands: []string{"nod"}}})
	var pc wire.PairCodeBody
	r.expect(wire.KindPairCode, &pc)
	return r, pc.Code
}

func (u *user) pair(code string) int {
	resp, err := u.c.Get(u.server + "/pair?code=" + code)
	if err != nil {
		u.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (u *user) pairedIDs() string {
	_, body := u.do("GET", "/api/robots", "", false)
	var views []robotView
	json.Unmarshal([]byte(body), &views)
	var ids []string
	for _, v := range views {
		ids = append(ids, v.ID)
	}
	return strings.Join(ids, ",")
}

func TestPrivateRobotOnlyForItsOwner(t *testing.T) {
	f := newFakeIssuer(t)
	ts, _, m := newManagedServer(t, f, "")
	ema, jan, anon := newUser(t, ts.URL), newUser(t, ts.URL), newUser(t, ts.URL)
	ema.signIn(f, "ema", "ema@example.com", "Ema")
	jan.signIn(f, "jan", "jan@example.com", "Jan")

	const id = "stackchan-0a1b2c3d4e50"
	m.set(id, "tok", f.ts.URL+"|ema", false)
	rb, code := robotWithCode(t, ts, "tok", id)

	// Private by default: the code from its screen pairs nobody but its owner.
	if got := anon.pair(code); got != http.StatusForbidden {
		t.Fatalf("anonymous pairs a private robot: %d", got)
	}
	if got := jan.pair(code); got != http.StatusForbidden {
		t.Fatalf("another account pairs a private robot: %d", got)
	}
	// The owner's signed-in browser has it without a code.
	if got := ema.pairedIDs(); got != id {
		t.Fatalf("owner's robots: %q", got)
	}

	// Made public on the manager: when the robot connects again, its code pairs anyone.
	m.set(id, "tok-pub", f.ts.URL+"|ema", true) // a new setup: a new token (no cached answer)
	rb.ws.Close()
	_, code = robotWithCode(t, ts, "tok-pub", id)
	if got := anon.pair(code); got != http.StatusSeeOther {
		t.Fatalf("anonymous pairs a public robot: %d", got)
	}
	if got := anon.pairedIDs(); got != id {
		t.Fatalf("anonymous after pairing: %q", got)
	}
	// Private again: the others are unpaired, the owner keeps it.
	m.set(id, "tok-priv", f.ts.URL+"|ema", false)
	robotWithCode(t, ts, "tok-priv", id)
	if got := anon.pairedIDs(); got != "" {
		t.Fatalf("anonymous still paired after private: %q", got)
	}
	if got := ema.pairedIDs(); got != id {
		t.Fatalf("owner lost the robot: %q", got)
	}
}

func TestServerRobotsBelongToAdmins(t *testing.T) {
	f := newFakeIssuer(t)
	ts, _ := newSignInServer(t, f, "")
	_, code := robotWithCode(t, ts, testToken, "stackchan-owner00001")
	anon := newUser(t, ts.URL)
	if got := anon.pair(code); got != http.StatusForbidden {
		t.Fatalf("anonymous pairs the server's robot: %d", got)
	}
	boss := newUser(t, ts.URL)
	boss.signIn(f, "boss", "boss@example.com", "Boss")
	if got := boss.pairedIDs(); got != "stackchan-owner00001" {
		t.Fatalf("admin's robots: %q", got)
	}
}

func TestAnonymousCannotAskToJoin(t *testing.T) {
	f := newFakeIssuer(t)
	ts, _ := newSignInServer(t, f, "")
	anon := newUser(t, ts.URL)
	if code, _ := anon.do("POST", "/api/join", "", true); code != http.StatusUnauthorized {
		t.Fatalf("anonymous join request: %d", code)
	}
}
