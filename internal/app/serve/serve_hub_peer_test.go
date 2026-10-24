package serve

import (
	"bytes"
	"github.com/samsaffron/term-llm/internal/pairing"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

func TestHubPeerIngressRequiresLiveGrantAndDoesNotExposeCredentials(t *testing.T) {
	s := newHubServer(hub.NewRegistry(fakeHubResolver{}), nil)
	s.requireAuth = true
	s.token = "never-send-master"
	s.registrationToken = "never-send-registration"
	s.basePath = "/hub"
	live := true
	h := s.peerHandler(func(*http.Request) bool { return live }, func(string) bool { return true })
	for _, path := range []string{"/hub/", "/hub/api/registration-info", "/hub/api/auth/session"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		for _, secret := range []string{s.token, s.registrationToken} {
			if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Header().Get("Set-Cookie"), secret) {
				t.Fatalf("secret leaked from %s", path)
			}
		}
	}
	live = false
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/hub/", nil))
	if rec.Code != 401 {
		t.Fatalf("revoked grant returned %d", rec.Code)
	}
	// The new private ingress must not relax the public HTTP listener.
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/hub/api/nodes", nil))
	if rec.Code != 401 {
		t.Fatalf("HTTP auth bypass: %d", rec.Code)
	}
}

func TestHubPeerIngressScopesPathsAndInjectsOnlyNodeToken(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-only" {
			t.Error("missing server-side node auth")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get(hubNodeIDHeader) != "" {
			t.Error("untrusted authority reached node")
		}
		if r.URL.Path != "/chat/v1/responses" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: real-node-response\n\n")
	}))
	defer backend.Close()
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{{ID: "agent", URL: backend.URL, BasePath: "/chat", Token: "node-only"}}}), nil)
	h := s.peerHandler(func(*http.Request) bool { return true }, func(id string) bool { return id == "agent" })
	for _, p := range []string{"/api/register-node", "/api/connect", "/auth/login", "/api/nodes/test", "/node/other/v1/responses", "/node/agent/auth/security", "/node/agent/api/auth/credentials", "/node/agent/api/registration-info", "/node/agent/api/register-node", "/node/agent/api/connect"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", p, nil))
		if rec.Code != 403 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"/node/agent/%2e%2e/secret", "/node/agent/%2fsecret", "/node/agent/v1/responses?token=bad"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", p, nil))
		if rec.Code != 400 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	req := httptest.NewRequest("POST", "/node/agent/v1/responses", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer attacker")
	req.Header.Set("Cookie", "term_llm_hub_token=attacker")
	req.Header.Set(hubNodeIDHeader, "spoofed")
	req.Header.Set("Origin", "http://localhost:12345")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "real-node-response") {
		t.Fatalf("proxy %d %s", rec.Code, rec.Body.String())
	}
}

func TestHubPeerPrivateLinkFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := pairing.Link{Broker: "http://localhost:8091", SessionID: bytes.Repeat([]byte{4}, 16), HostKey: bytes.Repeat([]byte{1}, 32), HostNonce: bytes.Repeat([]byte{5}, 32), Secret: bytes.Repeat([]byte{3}, 32), ExpiresAt: time.Now().Add(time.Minute), Capability: strings.Repeat("a", 43)}
	path := filepath.Join(dir, "pair-link")
	if err := writePeerPairingLinkFile(path, link); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private link mode: %v %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pairing.ParseLink(strings.TrimSpace(string(data)))
	if err != nil || !bytes.Equal(parsed.Secret, link.Secret) {
		t.Fatalf("link parse: %v", err)
	}
	if err := writePeerPairingLinkFile(path, link); err == nil {
		t.Fatal("overwrote existing secret file")
	}
	public := filepath.Join(t.TempDir(), "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writePeerPairingLinkFile(filepath.Join(public, "secret"), link); err == nil {
		t.Fatal("wrote link into public directory")
	}
}
