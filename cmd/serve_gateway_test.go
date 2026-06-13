package cmd

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/contain"
)

// gatewayWithBackend spins up a fake agent serve and returns a gateway that
// proxies agent "alpha" to it with the given base path and a fixed token.
func gatewayWithBackend(t *testing.T, basePath string, h http.HandlerFunc) *gateway {
	t.Helper()
	backend := httptest.NewServer(h)
	t.Cleanup(backend.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return fakeGateway(host, map[string]contain.WebConfig{
		"alpha": {Port: port, Token: "tkn-123", BasePath: basePath},
	})
}

// fakeGateway builds a gateway whose discovery is backed by static maps so the
// proxy/list behaviour can be exercised without containers or a config dir.
func fakeGateway(host string, agents map[string]contain.WebConfig) *gateway {
	g := newGateway()
	g.host = host
	g.resolve = func(name string) (contain.WebConfig, error) {
		cfg, ok := agents[name]
		if !ok {
			return contain.WebConfig{}, fmt.Errorf("workspace %q not found", name)
		}
		return cfg, nil
	}
	g.list = func() ([]contain.ListEntry, error) {
		entries := make([]contain.ListEntry, 0, len(agents))
		for name := range agents {
			entries = append(entries, contain.ListEntry{Name: name, Status: "missing", Service: "app"})
		}
		return entries, nil
	}
	return g
}

func TestWidgetHostWarning(t *testing.T) {
	lookup := func(addrs []string, err error) func(string) ([]string, error) {
		return func(string) ([]string, error) { return addrs, err }
	}
	cases := []struct {
		name    string
		lookup  func(string) ([]string, error)
		wantMsg bool
	}{
		{"loopback-ipv4", lookup([]string{"127.0.0.1"}, nil), false},
		{"loopback-ipv6", lookup([]string{"::1"}, nil), false},
		{"loopback-both", lookup([]string{"127.0.0.1", "::1"}, nil), false},
		{"unresolved", lookup(nil, fmt.Errorf("no such host")), true},
		{"empty", lookup([]string{}, nil), true},
		{"public", lookup([]string{"93.184.216.34"}, nil), true},
		{"mixed-with-public", lookup([]string{"127.0.0.1", "10.0.0.5"}, nil), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := widgetHostWarning("widgets.localhost", tc.lookup)
			if tc.wantMsg && msg == "" {
				t.Fatalf("expected a warning, got none")
			}
			if !tc.wantMsg && msg != "" {
				t.Fatalf("expected no warning, got %q", msg)
			}
		})
	}
}

func TestGatewayHeadDoesNotClobberOrWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	const body = `<base href="/chat/"><script>window.TERM_LLM_UI_PREFIX="/chat";</script>`
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		// Mimic serveEmbeddedUIBytes: real Content-Length, no body on HEAD.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.WriteString(w, body)
	})
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/agent/alpha/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(body)) {
		t.Errorf("Content-Length = %q want %d (HEAD response must not be clobbered to 0)", cl, len(body))
	}
	if strings.Contains(buf.String(), "click-to-open") {
		t.Errorf("spurious drift warning on HEAD request: %s", buf.String())
	}
}

func TestGatewayTransportNoEnvProxy(t *testing.T) {
	// The gateway dials known agent hosts directly; honoring HTTP_PROXY could
	// route a token-injected request (with Authorization: Bearer) through an
	// external proxy and leak the per-agent token.
	if newGatewayTransport().Proxy != nil {
		t.Fatal("gateway transport must not use an environment proxy")
	}
}

func TestValidateWidgetHost(t *testing.T) {
	cases := []struct {
		host    string
		wantErr bool
	}{
		{"", false},
		{"widgets.localhost", false},
		{"widgets.example.com", false},
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"LocalHost", true}, // case-insensitive: would still shadow
	}
	for _, tc := range cases {
		if err := validateWidgetHost(tc.host); (err != nil) != tc.wantErr {
			t.Errorf("validateWidgetHost(%q) err=%v wantErr=%v", tc.host, err, tc.wantErr)
		}
	}
}

func TestValidateGatewayBind(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		port    int
		wantErr bool
	}{
		{"loopback-ipv4", "127.0.0.1", 8090, false},
		{"localhost", "localhost", 8090, false},
		{"loopback-ipv6", "::1", 8090, false},
		{"public-bind", "0.0.0.0", 8090, true},
		{"lan-bind", "192.168.1.20", 8090, true},
		{"bad-port-zero", "127.0.0.1", 0, true},
		{"bad-port-high", "127.0.0.1", 70000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGatewayBind(tc.host, tc.port)
			if tc.wantErr && err == nil {
				t.Fatalf("validateGatewayBind(%q,%d) = nil, want error", tc.host, tc.port)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateGatewayBind(%q,%d) = %v, want nil", tc.host, tc.port, err)
			}
		})
	}
}

func TestWebConfigCacheReusesUnchangedEntry(t *testing.T) {
	var loads int
	mtime := time.Unix(1000, 0)
	c := &webConfigCache{
		items: map[string]webConfigEntry{},
		load: func(name string) (contain.WebConfig, error) {
			loads++
			return contain.WebConfig{Port: "8081", Token: "t"}, nil
		},
		stat: func(name string) (time.Time, error) { return mtime, nil },
	}

	for i := 0; i < 3; i++ {
		if _, err := c.get("alpha"); err != nil {
			t.Fatalf("get #%d: %v", i, err)
		}
	}
	if loads != 1 {
		t.Fatalf("loads = %d, want 1 (cache should reuse the unchanged entry)", loads)
	}

	// A newer .env mtime invalidates the cache and forces a re-read.
	mtime = time.Unix(2000, 0)
	if _, err := c.get("alpha"); err != nil {
		t.Fatal(err)
	}
	if loads != 2 {
		t.Fatalf("loads = %d, want 2 (changed mtime should force re-read)", loads)
	}
}

func TestWebConfigCacheBypassesOnStatError(t *testing.T) {
	var loads int
	c := &webConfigCache{
		items: map[string]webConfigEntry{},
		load: func(name string) (contain.WebConfig, error) {
			loads++
			return contain.WebConfig{Port: "8081", Token: "t"}, nil
		},
		// Stat failing (e.g. .env vanished mid-flight) must not serve a stale
		// cached entry: fall back to loading every time.
		stat: func(name string) (time.Time, error) { return time.Time{}, fmt.Errorf("nope") },
	}
	for i := 0; i < 2; i++ {
		if _, err := c.get("alpha"); err != nil {
			t.Fatalf("get #%d: %v", i, err)
		}
	}
	if loads != 2 {
		t.Fatalf("loads = %d, want 2 (stat error must bypass the cache)", loads)
	}
}

func TestGatewayProxyInjectsTokenServerSide(t *testing.T) {
	var gotAuth, gotPath, gotQuery string
	var gotCookie string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello from agent")
	}))
	defer backend.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	g := fakeGateway(host, map[string]contain.WebConfig{
		"alpha": {Port: port, Token: "tkn-123", BasePath: "/chat"},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/v1/models?limit=5", nil)
	// A client-supplied Authorization/Cookie must never reach the backend; the
	// gateway injects the real per-agent token server-side.
	req.Header.Set("Authorization", "Bearer client-supplied-should-not-pass")
	req.Header.Set("Cookie", "term_llm_token=client-cookie")
	g.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer tkn-123" {
		t.Errorf("backend Authorization = %q want %q", gotAuth, "Bearer tkn-123")
	}
	if gotCookie != "" {
		t.Errorf("backend Cookie = %q want empty (client cookie must be stripped)", gotCookie)
	}
	if gotPath != "/chat/v1/models" {
		t.Errorf("backend path = %q want /chat/v1/models", gotPath)
	}
	if gotQuery != "limit=5" {
		t.Errorf("backend query = %q want limit=5", gotQuery)
	}
	if rec.Body.String() != "hello from agent" {
		t.Errorf("body = %q want hello from agent", rec.Body.String())
	}
}

func TestGatewayProxyUnknownAgentIs404(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/ghost/", nil)
	g.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404", rec.Code)
	}
}

func TestGatewayProxyNoTokenIsBadGateway(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{
		"alpha": {Port: "8081", Token: "", BasePath: "/chat"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/", nil)
	g.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d want 502", rec.Code)
	}
}

func TestGatewayProxyRejectsInvalidName(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{})
	rec := httptest.NewRecorder()
	// Path traversal style name must be rejected before any resolution.
	req := httptest.NewRequest(http.MethodGet, "/agent/..%2f..%2fetc/", nil)
	g.handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("invalid agent name proxied (status %d), want rejection", rec.Code)
	}
}

func TestGatewayListsAgentsWithoutLeakingTokens(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{
		"alpha": {Port: "8081", Token: "super-secret", BasePath: "/chat"},
		"beta":  {Port: "8082", Token: "", BasePath: "/chat"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	g.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret") {
		t.Fatalf("token leaked into /agents response: %s", rec.Body.String())
	}

	var resp struct {
		Agents []agentInfo `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /agents: %v body=%s", err, rec.Body.String())
	}
	byName := map[string]agentInfo{}
	for _, a := range resp.Agents {
		byName[a.Name] = a
	}
	if len(byName) != 2 {
		t.Fatalf("agents = %#v want 2", resp.Agents)
	}
	if !byName["alpha"].Reachable {
		t.Errorf("alpha should be reachable (has token)")
	}
	if byName["beta"].Reachable {
		t.Errorf("beta should not be reachable (no token)")
	}
	if byName["alpha"].ProxyPath != "/agent/alpha/" {
		t.Errorf("alpha proxy path = %q want /agent/alpha/", byName["alpha"].ProxyPath)
	}
	if byName["alpha"].Port != "8081" {
		t.Errorf("alpha port = %q want 8081", byName["alpha"].Port)
	}
}

func TestGatewayIndexListsAgents(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{
		"alpha": {Port: "8081", Token: "super-secret", BasePath: "/chat"},
		"beta":  {Port: "8082", Token: "", BasePath: "/chat"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	g.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q want text/html", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, "super-secret") {
		t.Fatalf("token leaked into index page: %s", body)
	}
	for _, want := range []string{"alpha", "beta", "/agent/alpha/"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q\nbody=%s", want, body)
		}
	}
}

func TestGatewayIndexOnlyAtRoot(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{})
	rec := httptest.NewRecorder()
	// A stray non-root path that is not an /agent/ or /agents route must 404,
	// not render the index.
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	g.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404 for %q", rec.Code, "/favicon.ico")
	}
}

// --- click-to-open: per-agent UI prefix rebasing through the gateway ---

func TestGatewayRewritesUIPrefixInHTML(t *testing.T) {
	html := `<!doctype html><html><head><meta charset="utf-8">` + "\n  " +
		`<base href="/chat/">` +
		`<script>window.TERM_LLM_UI_PREFIX="/chat";</script></head><body>hi</body></html>`
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, html)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/", nil)
	g.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<base href="/agent/alpha/">`) {
		t.Errorf("<base> not rebased to gateway mount:\n%s", body)
	}
	if !strings.Contains(body, `window.TERM_LLM_UI_PREFIX="/agent/alpha"`) {
		t.Errorf("UI prefix not rebased:\n%s", body)
	}
	if strings.Contains(body, "/chat") {
		t.Errorf("residual /chat after rebase:\n%s", body)
	}
}

func TestRebaseUIPrefixReportsHits(t *testing.T) {
	good := []byte(`<base href="/chat/"><script>window.TERM_LLM_UI_PREFIX="/chat";</script>`)
	out, baseHits, prefixHits := rebaseUIPrefix(good, "/chat", "/agent/alpha")
	if baseHits != 1 || prefixHits != 1 {
		t.Fatalf("good HTML hits = base %d prefix %d, want 1/1", baseHits, prefixHits)
	}
	if strings.Contains(string(out), "/chat") {
		t.Errorf("residual /chat after rebase: %s", out)
	}

	// serveui's emitted shape drifts (spaces around the assignment): the prefix
	// needle no longer matches and click-to-open would silently break.
	drifted := []byte(`<base href="/chat/"><script>window.TERM_LLM_UI_PREFIX = "/chat";</script>`)
	_, baseHits, prefixHits = rebaseUIPrefix(drifted, "/chat", "/agent/alpha")
	if baseHits != 1 {
		t.Errorf("drifted base hits = %d, want 1", baseHits)
	}
	if prefixHits != 0 {
		t.Errorf("drifted prefix hits = %d, want 0 (needle should miss)", prefixHits)
	}
}

// TestRebaseMatchesRealServeRender feeds the gateway's rebase needles the ACTUAL
// HTML the serve renders for its index — the <base> tag from serveui and the
// window.TERM_LLM_UI_PREFIX script from buildIndexHTML. This makes a drift in
// either emitter fail the build, instead of only logging a runtime warning.
func TestRebaseMatchesRealServeRender(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/chat", agentName: "x"}}
	rr := httptest.NewRecorder()
	srv.handleUI(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("handleUI status = %d", rr.Code)
	}
	rewritten, baseHits, prefixHits := rebaseUIPrefix(rr.Body.Bytes(), "/chat", "/agent/x")
	if baseHits < 1 {
		t.Errorf("<base> needle did not match real serve render — serveui HTML shape drifted")
	}
	if prefixHits < 1 {
		t.Errorf("TERM_LLM_UI_PREFIX needle did not match real serve render — buildIndexHTML shape drifted")
	}
	if bytes.Contains(rewritten, []byte(`href="/chat/"`)) || bytes.Contains(rewritten, []byte(`UI_PREFIX="/chat"`)) {
		t.Errorf("residual /chat after rebase:\n%s", rewritten)
	}
}

func TestGatewayWarnsWhenRebaseMatchesNothing(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	// HTML whose prefix needle has drifted: the rebase will match the <base>
	// tag but not the UI prefix, so the gateway must log a loud warning.
	drifted := `<base href="/chat/"><script>window.TERM_LLM_UI_PREFIX = "/chat";</script>`
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, drifted)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/", nil)
	g.handler().ServeHTTP(rec, req)

	logged := buf.String()
	if !strings.Contains(logged, "alpha") || !strings.Contains(strings.ToLower(logged), "warn") {
		t.Fatalf("expected a loud warning naming the agent, got: %q", logged)
	}
}

func TestGatewayDoesNotWarnOnCleanHTML(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	clean := `<base href="/chat/"><script>window.TERM_LLM_UI_PREFIX="/chat";</script>`
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, clean)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/", nil)
	g.handler().ServeHTTP(rec, req)

	if logged := buf.String(); strings.TrimSpace(logged) != "" {
		t.Fatalf("unexpected warning on clean HTML: %q", logged)
	}
}

func TestGatewayRewritesGzippedHTML(t *testing.T) {
	htmlBody := `<meta charset="utf-8"><base href="/chat/">` +
		`<script>window.TERM_LLM_UI_PREFIX="/chat";</script>`
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// The real serve gzips the UI when the request advertises gzip. The
		// gateway must still see decompressed HTML in order to rebase it.
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, htmlBody)
			gz.Close()
			return
		}
		io.WriteString(w, htmlBody)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/", nil)
	// Client advertises gzip; the gateway must take ownership of encoding so it
	// can rebase the body.
	req.Header.Set("Accept-Encoding", "gzip")
	g.handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `window.TERM_LLM_UI_PREFIX="/agent/alpha"`) {
		t.Errorf("gzipped HTML was not rebased (gateway likely forwarded client Accept-Encoding):\n%q", body)
	}
	if strings.Contains(body, "/chat") {
		t.Errorf("residual /chat after gzip rebase:\n%q", body)
	}
}

func TestGatewayDoesNotRewriteNonHTML(t *testing.T) {
	js := `const P = window.TERM_LLM_UI_PREFIX; fetch("/chat/v1/models");`
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		io.WriteString(w, js)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/app-core.js", nil)
	g.handler().ServeHTTP(rec, req)

	if rec.Body.String() != js {
		t.Fatalf("non-HTML body was rewritten:\n got=%q\nwant=%q", rec.Body.String(), js)
	}
}

func TestGatewayStripsXApiKeyAndInjectsAuth(t *testing.T) {
	var gotKey, gotAuth string
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/v1/models", nil)
	req.Header.Set("X-Api-Key", "client-key-should-not-pass")
	g.handler().ServeHTTP(rec, req)

	if gotKey != "" {
		t.Errorf("client x-api-key reached backend: %q", gotKey)
	}
	if gotAuth != "Bearer tkn-123" {
		t.Errorf("backend Authorization = %q want Bearer tkn-123", gotAuth)
	}
}

func TestGatewayRejectsEncodedSlashTraversal(t *testing.T) {
	hit := false
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	// Call handleProxy directly so the ServeMux's own path cleaning does not
	// pre-empt the gateway's encoded-separator guard under test.
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/v1/..%2f..%2fadmin", nil)
	g.handleProxy(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", rec.Code)
	}
	if hit {
		t.Fatal("backend received an encoded-slash traversal request")
	}
}

func TestGatewayTimesOutHungBackendHeaders(t *testing.T) {
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		// Never send response headers within the timeout window.
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	// The gateway must give its proxy a real *http.Transport (not nil/Default)
	// so a hung backend can't hang the proxy. Shrink the header timeout so the
	// test stays fast.
	tr := g.proxy.Transport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 50 * time.Millisecond
	g.proxy.Transport = tr

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/slow", nil)
	g.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d want 502 (hung backend should trip the header timeout)", rec.Code)
	}
}

func TestGatewayStreamsSSEIncrementally(t *testing.T) {
	release := make(chan struct{})
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("backend ResponseWriter is not a Flusher")
			return
		}
		io.WriteString(w, "data: first\n\n")
		fl.Flush()
		// Block: the client must receive "first" before "second" is ever
		// written. If the proxy buffered the body, the read below would deadlock.
		<-release
		io.WriteString(w, "data: second\n\n")
		fl.Flush()
	})

	front := httptest.NewServer(g.handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/agent/alpha/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q want text/event-stream", ct)
	}

	br := bufio.NewReader(resp.Body)
	if line := readDataLine(t, br); line != "data: first" {
		t.Fatalf("first event = %q want %q", line, "data: first")
	}
	// We got the first event while the backend is still blocked → not buffered.
	close(release)
	if line := readDataLine(t, br); line != "data: second" {
		t.Fatalf("second event = %q want %q", line, "data: second")
	}
}

// readDataLine reads lines until it finds an SSE "data:" line and returns it.
func readDataLine(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE stream: %v", err)
		}
		if line = strings.TrimRight(line, "\r\n"); strings.HasPrefix(line, "data:") {
			return line
		}
	}
}

func TestGatewayRewritesLocationHeader(t *testing.T) {
	g := gatewayWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/chat/v1/foo")
		w.WriteHeader(http.StatusFound)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/alpha/old", nil)
	g.handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Location"); got != "/agent/alpha/v1/foo" {
		t.Fatalf("Location = %q want /agent/alpha/v1/foo", got)
	}
}
