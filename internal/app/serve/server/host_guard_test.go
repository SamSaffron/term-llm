package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuardHost(t *testing.T) {
	for _, tc := range []struct {
		name, bind, public, host string
		allowed                  bool
	}{
		{"loopback", "127.0.0.1", "", "127.0.0.1:8080", true},
		{"localhost", "127.0.0.1", "", "LOCALHOST.:8080", true},
		{"ipv6", "::1", "", "[::1]:8080", true},
		{"rebound loopback", "127.0.0.1", "", "attacker.example:8080", false},
		{"rebound wildcard", "0.0.0.0", "", "attacker.example:8080", false},
		{"public proxy", "127.0.0.1", "https://chat.example/ui/", "chat.example", true},
		{"wrong proxy", "127.0.0.1", "https://chat.example/ui/", "attacker.example", false},
		{"named bind", "my-server", "", "my-server:8080", true},
		{"absolute DNS bind", "my-server.example.", "", "my-server.example.:8080", true},
		{"absolute DNS bind canonical request", "MY-SERVER.EXAMPLE.", "", "my-server.example:8080", true},
		{"literal LAN wildcard", "0.0.0.0", "", "192.168.1.5:8080", true},
		{"literal LAN loopback", "127.0.0.1", "", "192.168.1.5:8080", false},
		{"unspecified", "0.0.0.0", "", "0.0.0.0:8080", false},
		{"malformed", "127.0.0.1", "", "localhost:bad:8080", false},
		{"userinfo", "127.0.0.1", "", "localhost@attacker.example:8080", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := GuardHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }), tc.bind, tc.public)
			r := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
			r.Host = tc.host
			r.Header.Set("X-Forwarded-Host", "localhost")
			r.Header.Set("Origin", "http://"+tc.host)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if called != tc.allowed {
				t.Errorf("called=%v status=%d; want allowed=%v", called, w.Code, tc.allowed)
			}
			if !tc.allowed && w.Code != http.StatusForbidden {
				t.Errorf("status=%d want403", w.Code)
			}
		})
	}
}

func BenchmarkGuardHost(b *testing.B) {
	h := GuardHost(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "127.0.0.1", "")
	r := httptest.NewRequest(http.MethodGet, "http://localhost:8080/", nil)
	w := httptest.NewRecorder()
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(w, r)
	}
}
