package server

import (
	"log/slog"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestLocalSetup(t *testing.T) {
	s := &Server{cfg: Config{RobotToken: "t", PublicURL: "http://192.168.1.10:8765"}}
	for _, c := range []struct {
		name, remote, host, xff string
		trusted                 int
		want                    bool
	}{
		{"this computer", "127.0.0.1:5000", "localhost:8765", "", 0, true},
		{"this computer, ipv6", "[::1]:5000", "[::1]:8765", "", 0, true},
		{"another computer", "192.168.1.20:5000", "192.168.1.10:8765", "", 0, false},
		{"loopback, LAN host name", "127.0.0.1:5000", "192.168.1.10:8765", "", 0, false},
		{"DNS rebinding", "127.0.0.1:5000", "evil.example:8765", "", 0, false},
		{"local proxy", "127.0.0.1:5000", "localhost:8765", "203.0.113.5", 0, false},
		{"trusted proxies", "127.0.0.1:5000", "localhost:8765", "", 1, false},
	} {
		s.cfg.TrustedProxies = c.trusted
		r := httptest.NewRequest("POST", "/api/setup/local", nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.localSetup(r); got != c.want {
			t.Errorf("%s: localSetup = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSetupLocalHandler(t *testing.T) {
	s := &Server{cfg: Config{RobotToken: "tok", PublicURL: "http://192.168.1.10:8765"}, log: slog.New(slog.DiscardHandler)}
	for _, c := range []struct {
		origin string
		want   int
	}{{"http://localhost:8765", 200}, {"https://evil.example", 403}, {"", 403}} {
		r := httptest.NewRequest("POST", "/api/setup/local", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:5000", "localhost:8765"
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		s.handleSetupLocal(w, r)
		if w.Code != c.want {
			t.Errorf("origin %q: %d, want %d", c.origin, w.Code, c.want)
		}
		if c.want == 200 && !regexpMatch(`"url":"ws://192.168.1.10:8765"`, w.Body.String()) {
			t.Errorf("body %s", w.Body.String())
		}
	}
}

func TestWifiParsers(t *testing.T) {
	if got := nmcliActiveWifi("Wired:802-3-ethernet:eth0\nHome\\:5G:802-11-wireless:wlp2s0\n"); got != "Home:5G" {
		t.Errorf("nmcliActiveWifi = %q", got)
	}
	out := "    Name                   : Wi-Fi\n    SSID                   : Home Net\n    BSSID                  : aa:bb\n    Profile                : Home Net\n"
	if got := netshField(out, "SSID"); got != "Home Net" {
		t.Errorf("SSID = %q", got)
	}
	if got := netshField("    Key Content            : secret pass\n", "Key Content"); got != "secret pass" {
		t.Errorf("Key Content = %q", got)
	}
}

func regexpMatch(pattern, s string) bool {
	return regexp.MustCompile(regexp.QuoteMeta(pattern)).MatchString(s)
}
