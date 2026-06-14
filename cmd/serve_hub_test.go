package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/hub"
)

// hubWithBackend spins up a fake node serve and returns a hub fronting it as
// node "alpha" with the given base path and a fixed token.
func hubWithBackend(t *testing.T, basePath string, h http.HandlerFunc) *hubServer {
	t.Helper()
	backend := httptest.NewServer(h)
	t.Cleanup(backend.Close)
	return newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{{
		ID:       "alpha",
		Name:     "Alpha",
		Source:   hub.SourceConfig,
		URL:      backend.URL,
		BasePath: basePath,
		Token:    "tkn-123",
	}}}), nil)
}

type fakeHubResolver struct{ nodes []hub.Node }

func (f fakeHubResolver) Source() string             { return hub.SourceConfig }
func (f fakeHubResolver) Nodes() ([]hub.Node, error) { return f.nodes, nil }

func TestHubProxyInjectsTokenAndStripsCredentials(t *testing.T) {
	var gotAuth, gotCookie, gotAPIKey, gotPath, gotQuery, gotFwd string
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotFwd = r.Header.Get("X-Forwarded-Host")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		io.WriteString(w, "hello from node")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/node/alpha/v1/models?limit=5", nil)
	// Client-supplied credentials must never reach the node; the hub injects
	// the real per-node token server-side.
	req.Header.Set("Authorization", "Bearer client-supplied")
	req.Header.Set("Cookie", "term_llm_token=client-cookie")
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("X-Forwarded-Host", "evil.example.com")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer tkn-123" {
		t.Errorf("backend Authorization = %q, want injected token", gotAuth)
	}
	if gotCookie != "" || gotAPIKey != "" || gotFwd != "" {
		t.Errorf("credentials leaked: cookie=%q apikey=%q fwd=%q", gotCookie, gotAPIKey, gotFwd)
	}
	if gotPath != "/chat/v1/models" || gotQuery != "limit=5" {
		t.Errorf("backend path = %q query = %q", gotPath, gotQuery)
	}
}

func TestHubProxyRebasesAndInjectsHubContext(t *testing.T) {
	const body = `<html><head><base href="/chat/"><script>window.TERM_LLM_UI_PREFIX="/chat";</script></head><body></body></html>`
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
	})

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/node/alpha/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := rec.Body.String()
	if !strings.Contains(got, `<base href="/node/alpha/">`) {
		t.Errorf("base tag not rebased: %s", got)
	}
	if !strings.Contains(got, `window.TERM_LLM_UI_PREFIX="/node/alpha"`) {
		t.Errorf("UI prefix not rebased: %s", got)
	}
	if !strings.Contains(got, `window.TERM_LLM_HUB={"nodeId":"alpha","nodeName":"Alpha","url":"/"}`) {
		t.Errorf("hub context not injected: %s", got)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(got)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(got))
	}
}

func TestHubProxyLeavesNonHTMLUntouched(t *testing.T) {
	const body = `data: {"x":"/chat"}` + "\n\n"
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	})

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/node/alpha/v1/responses", nil))
	if rec.Body.String() != body {
		t.Errorf("SSE body rewritten: %q", rec.Body.String())
	}
}

func TestHubProxyRewritesLocation(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/chat/", http.StatusMovedPermanently)
	})

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/node/alpha/chat", nil))
	if loc := rec.Header().Get("Location"); loc != "/node/alpha/" {
		t.Errorf("Location = %q, want /node/alpha/", loc)
	}
}

func TestHubProxyUnknownNodeIs404(t *testing.T) {
	s := newHubServer(hub.NewRegistry(), nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/node/ghost/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHubProxyRejectsEncodedSeparatorsAndTraversal(t *testing.T) {
	s := newHubServer(hub.NewRegistry(), nil)
	for _, path := range []string{"/node/a%2fb/", "/node/alpha/..%2fetc"} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, rec.Code)
		}
	}
	// Go's ServeMux redirects dot-dot paths to their cleaned form before the
	// handler runs; hit the handler directly to verify its own guard for
	// requests that bypass mux cleaning.
	rec := httptest.NewRecorder()
	s.handleNodeProxy(rec, httptest.NewRequest(http.MethodGet, "/node/alpha/../escape", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("dot-dot status = %d, want 400", rec.Code)
	}
}

func TestHubBareNodePathRedirects(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/node/alpha?x=1", nil))
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/node/alpha/?x=1" {
		t.Errorf("Location = %q", loc)
	}
}

func TestHubNodesAPIDoesNotLeakTokens(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok","agent":"alpha-agent"}`)
	})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "tkn-123") {
		t.Fatal("node token leaked into /api/nodes response")
	}
	var resp struct {
		Nodes []hubNodeView `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("nodes = %+v", resp.Nodes)
	}
	n := resp.Nodes[0]
	if !n.HasToken || n.ProxyPath != "/node/alpha/" {
		t.Errorf("node view = %+v", n)
	}
	if !n.Status.Reachable || n.Status.Agent != "alpha-agent" {
		t.Errorf("status = %+v, want reachable with agent", n.Status)
	}
}

func TestHubAddTestRemoveNodeFlow(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer backend.Close()

	store := hub.NewStore(filepath.Join(t.TempDir(), "nodes.json"))
	s := newHubServer(hub.NewRegistry(store), store)
	h := s.handler()

	// Test connection (does not persist).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/test",
		strings.NewReader(`{"url":"`+backend.URL+`/chat","token":"t"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reachable":true`) {
		t.Fatalf("test status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Add.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/nodes",
		strings.NewReader(`{"name":"Beta","url":"`+backend.URL+`/chat","token":"beta-token"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add status = %d body=%s", rec.Code, rec.Body.String())
	}
	if node, ok := s.registry.Lookup("beta"); !ok || node.Token != "beta-token" {
		t.Fatalf("added node not resolvable: %+v %v", node, ok)
	}

	// Remove.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/nodes/beta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("remove status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := s.registry.Lookup("beta"); ok {
		t.Fatal("removed node still resolvable")
	}
}

func TestHubRejectsCrossSiteBrowserRequests(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("cross-site request reached backend")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/node/alpha/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("proxy cross-site status = %d, want 403", rec.Code)
	}

	store := hub.NewStore(filepath.Join(t.TempDir(), "nodes.json"))
	s = newHubServer(hub.NewRegistry(store), store)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{"url":"http://127.0.0.1:1/chat"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("api cross-site status = %d, want 403", rec.Code)
	}
}

func TestHubIndexBranding(t *testing.T) {
	s := newHubServer(hub.NewRegistry(), hub.NewStore(filepath.Join(t.TempDir(), "nodes.json")))
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "term-llm Hub") && !strings.Contains(body, `term-llm <span class="hub-brand-accent">Hub</span>`) {
		t.Error("dashboard missing term-llm Hub branding")
	}
	if !strings.Contains(body, "addNodeModal") {
		t.Error("dashboard missing Add Node UI while store is enabled")
	}
}

func TestHubTransportNoEnvProxy(t *testing.T) {
	// The hub dials known node hosts directly; honoring HTTP_PROXY could
	// route a token-injected request through an external proxy and leak the
	// per-node token.
	if newHubTransport().Proxy != nil {
		t.Fatal("hub transport must not use an environment proxy")
	}
}

func TestValidateHubBind(t *testing.T) {
	cases := []struct {
		host    string
		port    int
		wantErr bool
	}{
		{"127.0.0.1", 8090, false},
		{"localhost", 8090, false},
		{"::1", 8090, false},
		{"0.0.0.0", 8090, true},
		{"192.168.1.20", 8090, true},
		{"127.0.0.1", 0, true},
		{"127.0.0.1", 70000, true},
	}
	for _, tc := range cases {
		if err := validateHubBind(tc.host, tc.port); (err != nil) != tc.wantErr {
			t.Errorf("validateHubBind(%q,%d) err=%v wantErr=%v", tc.host, tc.port, err, tc.wantErr)
		}
	}
}

func TestHubHeadDoesNotClobberContentLength(t *testing.T) {
	const body = `<base href="/chat/">`
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.WriteString(w, body)
	})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/node/alpha/", nil))
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(body)) {
		t.Errorf("HEAD Content-Length = %q, want %d", cl, len(body))
	}
}

func TestServeHealthIdentityGating(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{
		requireAuth: true,
		token:       "node-token",
		agentName:   "jarvis",
		ui:          true,
	}}

	// Unauthenticated: bare status only.
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "jarvis") {
		t.Errorf("unauthenticated healthz leaked identity: %s", rec.Body.String())
	}

	// With the bearer token: identity fields included.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer node-token")
	srv.handleHealth(rec, req)
	var resp struct {
		Status       string   `json:"status"`
		Agent        string   `json:"agent"`
		Version      string   `json:"version"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Agent != "jarvis" || resp.Version == "" || len(resp.Capabilities) == 0 {
		t.Errorf("authed healthz = %+v, want identity fields", resp)
	}

	// Wrong token: no identity.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	srv.handleHealth(rec, req)
	if strings.Contains(rec.Body.String(), "jarvis") {
		t.Errorf("wrong-token healthz leaked identity: %s", rec.Body.String())
	}
}

func TestServeWebHubFlagsInjectContext(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{
		basePath:    "/chat",
		hubURL:      "http://127.0.0.1:8090/",
		hubNodeID:   "jarvis",
		hubNodeName: "Jarvis",
	}}
	html := string(srv.buildIndexHTML())
	if !strings.Contains(html, `window.TERM_LLM_HUB={"nodeId":"jarvis","nodeName":"Jarvis","url":"http://127.0.0.1:8090/"}`) {
		t.Errorf("index missing hub context: %s", html[:min(600, len(html))])
	}
}

// Guard against the serveui shapes drifting away from the hub's rebase
// needles (see hubRebaseUIPrefix).
func TestHubRebaseNeedlesMatchServeOutput(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{basePath: "/chat"}}
	html := srv.buildIndexHTML()
	rewritten, baseHits, prefixHits := hubRebaseUIPrefix(html, "/chat", "/node/x")
	if baseHits == 0 || prefixHits == 0 {
		t.Fatalf("rebase needles drifted: baseHits=%d prefixHits=%d", baseHits, prefixHits)
	}
	if !strings.Contains(string(rewritten), `<base href="/node/x/">`) {
		t.Error("rebased html missing new base tag")
	}
}
