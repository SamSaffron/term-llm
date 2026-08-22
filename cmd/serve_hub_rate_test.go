package cmd

import (
	"net/http/httptest"
	"testing"
)

func TestHubClientPeerResolverTrustBoundary(t *testing.T) {
	resolver, err := newHubClientPeerResolver([]string{"127.0.0.1", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, remote, forwarded, want string
	}{
		{"untrusted peer ignores header", "192.0.2.10:1234", "198.51.100.9", "192.0.2.10"},
		{"trusted proxy uses client", "127.0.0.1:1234", "198.51.100.9", "198.51.100.9"},
		{"trusted chain walks from right", "127.0.0.1:1234", "203.0.113.8, 10.1.2.3", "203.0.113.8"},
		{"spoofed left entry loses to real client", "127.0.0.1:1234", "198.51.100.50, 203.0.113.8", "203.0.113.8"},
		{"malformed header fails closed", "127.0.0.1:1234", "not-an-ip", "127.0.0.1"},
		{"missing header uses proxy", "127.0.0.1:1234", "", "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "http://backend/", nil)
			request.RemoteAddr = tt.remote
			if tt.forwarded != "" {
				request.Header.Set("X-Forwarded-For", tt.forwarded)
			}
			if got := resolver.peer(request); got != tt.want {
				t.Fatalf("peer = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHubClientPeerResolverRejectsInvalidConfig(t *testing.T) {
	if _, err := newHubClientPeerResolver([]string{"not-a-network"}); err == nil {
		t.Fatal("accepted invalid trusted proxy")
	}
}

func TestHubAuthLimiterSeparatesForwardedClients(t *testing.T) {
	resolver, err := newHubClientPeerResolver([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	limiter := newHubAuthLimiter(nil)
	for i := 0; i < 5; i++ {
		request := httptest.NewRequest("POST", "http://backend/api/auth/login/begin", nil)
		request.RemoteAddr = "127.0.0.1:8090"
		request.Header.Set("X-Forwarded-For", "198.51.100.1")
		if !limiter.allow(resolver.peer(request)) {
			t.Fatalf("first client request %d denied", i)
		}
	}
	request := httptest.NewRequest("POST", "http://backend/api/auth/login/begin", nil)
	request.RemoteAddr = "127.0.0.1:8090"
	request.Header.Set("X-Forwarded-For", "198.51.100.2")
	if !limiter.allow(resolver.peer(request)) {
		t.Fatal("second forwarded client shared the first client's bucket")
	}
}
