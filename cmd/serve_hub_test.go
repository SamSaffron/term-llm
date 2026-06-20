package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestHubAuthProtectsDashboardAPIAndProxy(t *testing.T) {
	var gotAuth string
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, "ok")
	})
	s.requireAuth = true
	s.token = "hub-secret"
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz without auth status = %d, want 200", rec.Code)
	}

	for _, path := range []string{"/", "/api/nodes", "/node/alpha/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without auth status = %d, want 401", path, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/node/alpha/v1/models", nil)
	req.Header.Set("Authorization", "bearer hub-secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized proxy status = %d body=%q", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer tkn-123" {
		t.Fatalf("backend Authorization = %q, want node token injection", gotAuth)
	}
}

func TestHubAuthQueryTokenSetsCookie(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	s.requireAuth = true
	s.token = "hub-secret"
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?token=hub-secret", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("query token status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("redirect location = %q, want /", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != hubAuthCookieName || cookies[0].Value != "hub-secret" || !cookies[0].HttpOnly {
		t.Fatalf("auth cookie = %#v", cookies)
	}

	req := httptest.NewRequest(http.MethodGet, "/node/alpha/v1/models", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie-auth proxy status = %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestHubAuthBrowserNavigationShowsLoginPage(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	s.requireAuth = true
	s.token = "hub-secret"
	h := s.handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("browser login status = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"term-llm Hub", "Hub token", `name="token"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("login page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "invalid hub authentication credentials") || strings.Contains(body, "invalid_api_key") {
		t.Fatalf("login page should not show API JSON error: %s", body)
	}
}

func TestHubAuthInvalidBrowserTokenShowsLoginError(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	s.requireAuth = true
	s.token = "hub-secret"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?token=wrong", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid browser token status = %d, want 401", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not accepted") || !strings.Contains(body, "Hub token") {
		t.Fatalf("invalid token login body = %q", body)
	}
}

func TestHubAuthAPIStillReturnsJSONError(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	s.requireAuth = true
	s.token = "hub-secret"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	req.Header.Set("Accept", "text/html")
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API status = %d, want 401", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("API content-type = %q, want JSON", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "invalid_api_key") || strings.Contains(body, "Hub token") {
		t.Fatalf("API body = %q", body)
	}
}

func TestHubAuthSkipsReverseConnectNodeAuth(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	s.requireAuth = true
	s.token = "hub-secret"

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/connect", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want node-auth 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hub authentication") {
		t.Fatalf("/api/connect should be gated by node auth, got body %q", rec.Body.String())
	}
}

func TestValidateHubBindAllowsPublicOnlyWithAuth(t *testing.T) {
	if err := validateHubBind("0.0.0.0", 8090, true); err != nil {
		t.Fatalf("public bind with auth: %v", err)
	}
	if err := validateHubBind("0.0.0.0", 8090, false); err == nil {
		t.Fatal("expected public bind without auth to fail")
	}
	if err := validateHubBind("127.0.0.1", 8090, false); err != nil {
		t.Fatalf("loopback bind without auth: %v", err)
	}
}

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

func TestHubProxySkipsRebaseForOversizedHTML(t *testing.T) {
	body := strings.Repeat("x", hubHTMLRebaseMaxBytes+2)
	target := &hubProxyTarget{nodeID: "alpha", nodeName: "Alpha", basePath: "/chat", mount: "/node/alpha"}
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(withHubProxyTarget(context.Background(), target))
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
	if err := hubRebaseProxyResponse(resp); err != nil {
		t.Fatalf("hubRebaseProxyResponse: %v", err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != body {
		t.Fatalf("oversized body changed: len=%d want=%d", len(data), len(body))
	}
	if strings.Contains(string(data), "TERM_LLM_HUB") {
		t.Fatal("oversized HTML should not be buffered and injected")
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

func TestHubRewriteLocationHeaderVariants(t *testing.T) {
	target := &hubProxyTarget{nodeID: "alpha", nodeName: "Alpha", scheme: "http", host: "127.0.0.1:8081", basePath: "/chat", mount: "/node/alpha"}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"base root", "/chat", "/node/alpha"},
		{"base child", "/chat/files/a.png?download=1", "/node/alpha/files/a.png?download=1"},
		{"absolute same origin", "http://127.0.0.1:8081/chat/files/a.png?x=1#frag", "/node/alpha/files/a.png?x=1#frag"},
		{"out of base", "/login?next=/chat", "/node/alpha/login?next=/chat"},
		{"external absolute unchanged", "https://example.com/chat", "https://example.com/chat"},
		{"relative unchanged", "../login", "../login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{"Location": []string{tc.in}}}
			hubRewriteLocationHeader(resp, target)
			if got := resp.Header.Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
		})
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

func TestHubStripHopByHopHeaders(t *testing.T) {
	h := http.Header{
		"Connection":          []string{"keep-alive, X-Remove"},
		"Keep-Alive":          []string{"timeout=5"},
		"Proxy-Authenticate":  []string{"Basic"},
		"Proxy-Authorization": []string{"Basic secret"},
		"Te":                  []string{"trailers"},
		"Trailer":             []string{"X-Trailer"},
		"Transfer-Encoding":   []string{"chunked"},
		"Upgrade":             []string{"websocket"},
		"X-Remove":            []string{"yes"},
		"X-Keep":              []string{"ok"},
	}
	hubStripHopByHopHeaders(h)
	for _, key := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "X-Remove"} {
		if h.Get(key) != "" {
			t.Fatalf("%s header survived: %q", key, h.Get(key))
		}
	}
	if h.Get("X-Keep") != "ok" {
		t.Fatalf("end-to-end header removed: %q", h.Get("X-Keep"))
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

func TestHubNodesAPIDiagnostics(t *testing.T) {
	webOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok","capabilities":["web"]}`)
	}))
	defer webOnly.Close()
	jobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok","capabilities":["web","jobs"]}`)
	}))
	defer jobs.Close()

	nodes := []hub.Node{
		{ID: "origin", Name: "Origin", Source: hub.SourceConfig, URL: jobs.URL, BasePath: "/chat", Token: "origin-token", Delegation: &hub.DelegationPolicy{Enabled: true, To: []string{"target"}, Workdir: "/work"}},
		{ID: "target", Name: "Target", Source: hub.SourceConfig, URL: webOnly.URL, BasePath: "/chat", Token: "target-token", Delegation: &hub.DelegationPolicy{Enabled: true, AcceptFrom: []string{"other"}, Workdir: "/work"}},
		{ID: "nowork", Name: "NoWork", Source: hub.SourceConfig, Connection: "reverse", BasePath: "/chat", Token: "nowork-token", Delegation: &hub.DelegationPolicy{Enabled: true}},
		{ID: "notoken", Name: "NoToken", Source: hub.SourceConfig, URL: jobs.URL, BasePath: "/chat"},
	}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}), nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes []hubNodeView `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byID := map[string]hubNodeView{}
	for _, n := range resp.Nodes {
		byID[n.ID] = n
	}
	assertDiagnosticCode(t, byID["nowork"], "reverse_disconnected")
	assertDiagnosticCode(t, byID["nowork"], "delegation_missing_workdir")
	assertDiagnosticCode(t, byID["notoken"], "missing_token")
	assertDiagnosticCode(t, byID["target"], "delegation_jobs_missing")
	assertDiagnosticCode(t, byID["origin"], "delegation_policy_mismatch")
}

func assertDiagnosticCode(t *testing.T, n hubNodeView, code string) {
	t.Helper()
	for _, d := range n.Diagnostics {
		if d.Code == code {
			return
		}
	}
	t.Fatalf("node %q diagnostics missing %q: %+v", n.ID, code, n.Diagnostics)
}

func TestHubIndexIncludesDiagnosticsUI(t *testing.T) {
	s := newHubServer(hub.NewRegistry(), nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{"node-diagnostics", "diagnostic-label", "n.diagnostics"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
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

func TestHubNodeAPIClientDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		_, _ = w.Write([]byte(`{"id":"redirected"}`))
	}))
	defer target.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/internal", http.StatusFound)
	}))
	defer backend.Close()

	node := hub.Node{ID: "alpha", URL: backend.URL, BasePath: "/chat", Token: "node-token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	var out struct{ ID string }
	err := s.doNodeJSON(t.Context(), node, http.MethodPost, "/v2/jobs", map[string]string{"name": "demo"}, &out)
	if redirected.Load() {
		t.Fatal("node API client followed redirect to target server")
	}
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("doNodeJSON err = %v, want HTTP 302", err)
	}
}

func TestValidateHubBind(t *testing.T) {
	cases := []struct {
		host        string
		port        int
		requireAuth bool
		wantErr     bool
	}{
		{"127.0.0.1", 8090, false, false},
		{"localhost", 8090, false, false},
		{"::1", 8090, false, false},
		{"0.0.0.0", 8090, false, true},
		{"192.168.1.20", 8090, false, true},
		{"0.0.0.0", 8090, true, false},
		{"127.0.0.1", 0, true, true},
		{"127.0.0.1", 70000, true, true},
	}
	for _, tc := range cases {
		if err := validateHubBind(tc.host, tc.port, tc.requireAuth); (err != nil) != tc.wantErr {
			t.Errorf("validateHubBind(%q,%d,%v) err=%v wantErr=%v", tc.host, tc.port, tc.requireAuth, err, tc.wantErr)
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
