package cmd

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/samsaffron/term-llm/internal/widgets"
	"github.com/spf13/cobra"
)

func TestWebPasskeyAuthenticationBoundary(t *testing.T) {
	runtime := newTestPasskeyHub(t, "/ui").passkey
	auth := newWebPasskeyHandler(runtime)
	server := &serveServer{cfg: serveServerConfig{ui: true, requireAuth: true, token: "must-not-work", basePath: "/ui"}, browserAuth: auth}
	server.widgetsMgr = widgets.NewManager(t.TempDir(), "/ui")
	handler := server.httpHandler()
	issued, err := runtime.sessions.Create("credential")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, method, cookie, origin, fetchSite string
		want                                          int
	}{
		{name: "anonymous API", path: "/v1/models", want: 401},
		{name: "legacy bearer", path: "/v1/models", cookie: "term_llm_token", want: 401},
		{name: "Hub cookie", path: "/api/auth/session", cookie: hubSessionCookieName, want: 401},
		{name: "Web session", path: "/api/auth/session", cookie: "term_llm_web_session", want: 200},
		{name: "security page", path: "/auth/security", cookie: "term_llm_web_session", want: 200},
		{name: "OAuth GET verifies state", path: "/v1/mcp/oauth/callback", want: 400},
		{name: "OAuth POST protected", path: "/v1/mcp/oauth/callback", method: "POST", want: 401},
		{name: "widget cookie mutation reaches proxy", path: "/widgets/absent/", method: "POST", cookie: "term_llm_web_session", origin: runtime.endpoint.Origin, want: 404},
		{name: "widget cross-origin denied", path: "/widgets/absent/", method: "POST", cookie: "term_llm_web_session", origin: "null", want: 403},
		{name: "OPTIONS never forwarded", path: "/widgets/example/", method: "OPTIONS", want: 405},
		{name: "chat asset protected", path: "/dist/app.js", want: 401},
		{name: "auth CSS public", path: "/dist/hub.css", want: 200},
		{name: "health", path: "/healthz", want: 200},
		{name: "setup", path: "/auth/setup", want: 200},
		{name: "auth asset", path: "/dist/hub.js", want: 200},
		{name: "Hub connect not public", path: "/api/connect", want: 401},
		{name: "Hub registration not public", path: "/api/register-node", method: "POST", want: 401},
		{name: "missing origin", path: "/api/auth/sessions/revoke-others", method: "POST", cookie: "term_llm_web_session", want: 403},
		{name: "cross origin", path: "/v1/responses", method: "POST", cookie: "term_llm_web_session", origin: "https://evil.example", want: 403},
		{name: "same site attack", path: "/v1/responses", method: "POST", cookie: "term_llm_web_session", origin: runtime.endpoint.Origin, fetchSite: "same-site", want: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = "GET"
			}
			r := httptest.NewRequest(method, "http://localhost:8090/ui"+tc.path, strings.NewReader(`{}`))
			r.Header.Set("Authorization", "Bearer must-not-work")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: tc.cookie, Value: issued.Token})
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	nav := httptest.NewRequest("GET", "http://localhost:8090/ui/", nil)
	nav.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, nav)
	if w.Code != 303 || w.Header().Get("Location") != "/ui/auth/setup" {
		t.Fatalf("navigation=%d %s", w.Code, w.Header().Get("Location"))
	}
	// A valid Web cookie is accepted without a bearer header by existing API wrappers.
	called := false
	guarded := auth.passkeyAuth(server.auth(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }))
	r := httptest.NewRequest("POST", "http://localhost:8090/v1/responses", nil)
	r.Header.Set("Origin", runtime.endpoint.Origin)
	r.AddCookie(&http.Cookie{Name: "term_llm_web_session", Value: issued.Token})
	guarded.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("valid cookie did not reach Web handler")
	}
}

func TestWebPasskeyModeValidation(t *testing.T) {
	oldToken, oldURL := serveToken, servePublicURL
	t.Cleanup(func() { serveToken, servePublicURL = oldToken, oldURL })
	t.Setenv("TERM_LLM_SERVE_TOKEN", "")
	serveToken = ""
	servePublicURL = "http://localhost:8080/ui/"
	endpoint, err := resolveWebPasskeyEndpoint(serveCmd)
	if err != nil || endpoint.BasePath != "/ui" {
		t.Fatalf("endpoint=%+v err=%v", endpoint, err)
	}
	serveToken = "explicit"
	if _, err := resolveWebPasskeyEndpoint(serveCmd); err == nil {
		t.Fatal("accepted bearer compatibility")
	}
	serveToken = ""
	servePublicURL = "http://127.0.0.1:8080/ui/"
	if _, err := resolveWebPasskeyEndpoint(serveCmd); err == nil {
		t.Fatal("accepted loopback IP")
	}
	mode, err := resolveServeAuthMode(true, "passkey", false, false)
	if err != nil || mode != "passkey" {
		t.Fatalf("mode=%s err=%v", mode, err)
	}
	if _, err := resolveServeAuthMode(true, "passkey", true, true); err == nil {
		t.Fatal("accepted conflicting no-auth")
	}
}

func TestBrowserPasskeyStartupFailureReleasesLocks(t *testing.T) {
	endpoint, err := passkeyauth.ParseEndpoint(passkeyauth.EndpointOptions{PublicURL: "http://localhost/ui/"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	authFile := filepath.Join(t.TempDir(), "auth", "auth.json")
	failure := errors.New("bootstrap unavailable")
	opts := browserPasskeyOptions{userName: "Web operator", bootstrap: func(*cobra.Command, bool) ([]byte, string, error) { return nil, "", failure }}
	for i := 0; i < 2; i++ {
		_, _, _, err := openBrowserPasskeys(cmd, endpoint, authFile, opts)
		if !errors.Is(err, failure) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
}

func TestWebPasskeyMountAndReturnPaths(t *testing.T) {
	auth := newWebPasskeyHandler(newTestPasskeyHub(t, "/ui").passkey)
	cmd := &cobra.Command{}
	for _, tc := range []struct {
		path  string
		valid bool
	}{{"", true}, {"/ui", true}, {"/ui/", true}, {"/chat", false}} {
		if err := validateWebPasskeyConfigPath(cmd, auth.passkey.endpoint, tc.path); (err == nil) != tc.valid {
			t.Errorf("config path %q: %v", tc.path, err)
		}
	}
	for _, raw := range []string{"https://evil.example/", "//evil.example/", "/hub/private", "/ui/../private"} {
		if got := auth.passkey.endpoint.SafeReturnPath(raw); got != "/ui/" {
			t.Errorf("unsafe return %q: %q", raw, got)
		}
	}
	u := httptest.NewRequest("GET", "http://backend/session?x=1", nil).URL
	if got := auth.publicURLString(u); got != "/ui/session?x=1" {
		t.Errorf("return lost path or retained host: %s", got)
	}
}

func TestWebPasskeyCookieAndLegacyCleanup(t *testing.T) {
	auth := newWebPasskeyHandler(newTestPasskeyHub(t, "/ui").passkey)
	issued, err := auth.passkey.sessions.Create("credential")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	auth.setSessionCookie(w, issued)
	c := w.Result().Cookies()[0]
	if c.Name != "term_llm_web_session" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/ui/" {
		t.Fatalf("cookie=%+v", c)
	}
	auth.passkey.endpoint.Secure = true
	w = httptest.NewRecorder()
	auth.setSessionCookie(w, issued)
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS cookie not Secure")
	}
	r := httptest.NewRequest("GET", "http://localhost/ui/", nil)
	r.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "stale"})
	w = httptest.NewRecorder()
	auth.passkeyAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("legacy token authenticated") })).ServeHTTP(w, r)
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == "term_llm_token" && c.Path == "/ui" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("legacy cookie was not expired at its original path")
	}
}

func TestWebPasskeyUnsupportedTransports(t *testing.T) {
	oldURL, oldToken, oldConnect, oldHubURL, oldWebRTC, oldRegister, oldCORS := servePublicURL, serveToken, serveHubConnect, serveHubURL, serveWebRTC, serveHubRegister, serveCORSOrigins
	t.Cleanup(func() {
		servePublicURL, serveToken, serveHubConnect, serveHubURL, serveWebRTC, serveHubRegister, serveCORSOrigins = oldURL, oldToken, oldConnect, oldHubURL, oldWebRTC, oldRegister, oldCORS
	})
	t.Setenv("TERM_LLM_SERVE_TOKEN", "")
	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{"reverse case and whitespace", func() { serveHubConnect = " Reverse " }},
		{"Hub URL", func() { serveHubURL = "https://hub.example/" }},
		{"registration", func() { serveHubRegister = true }},
		{"WebRTC", func() { serveWebRTC = true }},
		{"CORS", func() { serveCORSOrigins = []string{"https://other.example"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			servePublicURL = "http://localhost:8080/ui/"
			serveToken = ""
			serveHubConnect = "direct"
			serveHubURL = ""
			serveWebRTC = false
			serveHubRegister = false
			serveCORSOrigins = nil
			tc.setup()
			if _, err := resolveWebPasskeyEndpoint(serveCmd); err == nil {
				t.Fatal("accepted incompatible transport")
			}
		})
	}
}
