package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/passkeyauth"
)

func configureTestPasskeyHub(t *testing.T, s *hubServer, base string) *hubServer {
	t.Helper()
	raw := "http://localhost:8090" + base + "/"
	endpoint, err := passkeyauth.ParseEndpoint(passkeyauth.EndpointOptions{PublicURL: raw, BasePath: base, BasePathExplicit: true})
	if err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(t.TempDir(), "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := passkeyauth.OpenStore(passkeyauth.StoreOptions{Path: filepath.Join(authDir, "auth.json"), RPID: endpoint.RPID, UserName: hubPasskeyUserName})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := passkeyauth.OpenSessions(passkeyauth.SessionsOptions{Path: filepath.Join(authDir, "sessions.json"), RPID: endpoint.RPID, UserID: store.User().ID, ValidCredential: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	bootstrap, err := passkeyauth.NewGrants(passkeyauth.GrantBootstrap, []byte("abcdefghijklmnopqrst"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	recovery, _ := passkeyauth.NewGrants(passkeyauth.GrantRecovery, nil, nil, nil)
	runtime, err := newHubPasskeyRuntime(endpoint, store, sessions, bootstrap, recovery, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.requireAuth = true
	s.authMode = "passkey"
	s.basePath = base
	s.passkey = runtime
	return s
}

func newTestPasskeyHub(t *testing.T, base string) *hubServer {
	return configureTestPasskeyHub(t, newHubServer(hub.NewRegistry(), nil), base)
}

func TestHubPasskeyUnauthorizedBehaviorAndBasePath(t *testing.T) {
	s := newTestPasskeyHub(t, "/hub")
	h := s.handler()
	api := httptest.NewRequest(http.MethodGet, "http://backend/hub/api/auth/session", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, api)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("session api=%d %s", w.Code, w.Body.String())
	}
	api = httptest.NewRequest(http.MethodGet, "http://backend/hub/api/auth/login/unregistered", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, api)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unregistered auth route bypassed middleware: %d %s", w.Code, w.Body.String())
	}
	api = httptest.NewRequest(http.MethodGet, "http://backend/hub/api/nodes", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, api)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("api=%d %s", w.Code, w.Body.String())
	}
	node := httptest.NewRequest(http.MethodGet, "http://backend/hub/node/x/api", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, node)
	if w.Code != 401 || w.Header().Get("X-Term-LLM-Login-URL") != "/hub/auth/login" || !strings.Contains(w.Body.String(), "hub_auth_required") {
		t.Fatalf("node=%d header=%q body=%s", w.Code, w.Header().Get("X-Term-LLM-Login-URL"), w.Body.String())
	}
	nav := httptest.NewRequest(http.MethodGet, "http://backend/hub/", nil)
	nav.Header.Set("Accept", "text/html")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, nav)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/hub/auth/setup" {
		t.Fatalf("nav=%d %q", w.Code, w.Header().Get("Location"))
	}
	for _, path := range []string{"/hub/api/connect", "/hub/api/register-node"} {
		method := http.MethodGet
		if strings.Contains(path, "register-node") {
			method = http.MethodPost
		}
		request := httptest.NewRequest(method, "http://backend"+path, nil)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if strings.Contains(response.Body.String(), "Hub passkey authentication") {
			t.Fatalf("machine-auth route %s was intercepted by operator auth: %s", path, response.Body.String())
		}
	}
	// Once setup is unavailable, login intents retain safe paths but strip legacy bearer query values.
	s.passkey.bootstrap, _ = passkeyauth.NewGrants(passkeyauth.GrantBootstrap, nil, nil, nil)
	secretNav := httptest.NewRequest(http.MethodGet, "http://backend/hub/node/alpha/?token=supersecret&safe=1", nil)
	secretNav.Header.Set("Accept", "text/html")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, secretNav)
	location := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || strings.Contains(location, "supersecret") || strings.Contains(location, "token") {
		t.Fatalf("secret navigation redirect=%d %q", w.Code, location)
	}
}
func TestHubPasskeyOnlyStandaloneAssetsArePublic(t *testing.T) {
	s := newTestPasskeyHub(t, "/hub")
	handler := s.handler()
	for _, path := range []string{"/hub/dist/hub.js", "/hub/dist/hub.css"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://backend"+path, nil))
		if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
			t.Errorf("public asset %s status=%d bytes=%d", path, recorder.Code, recorder.Body.Len())
		}
	}
	for _, path := range []string{"/hub/dist/app.js", "/hub/dist/chunks/vendor.js", "/hub/dist/hub.js.map", "/hub/api/nodes"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://backend"+path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("protected path %s status=%d, want 401", path, recorder.Code)
		}
	}
}

func TestHubPasskeyExpiredSessionSignalsMountedLogin(t *testing.T) {
	s := newTestPasskeyHub(t, "/hub")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.passkey.sessions = passkeyauth.NewSessions(func() time.Time { return now }, nil)
	issued, err := s.passkey.sessions.Create("credential")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(passkeyauth.SessionIdleLifetime + time.Second)
	req := httptest.NewRequest(http.MethodGet, "http://backend/hub/node/alpha/api", nil)
	req.AddCookie(&http.Cookie{Name: hubSessionCookieName, Value: issued.Token})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Term-LLM-Login-URL") != "/hub/auth/login" || !strings.Contains(w.Body.String(), "hub_auth_required") {
		t.Fatalf("%d header=%q body=%s", w.Code, w.Header().Get("X-Term-LLM-Login-URL"), w.Body.String())
	}
}

func TestHubPasskeyAuthBodyLimit(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	body := `{"code":"` + strings.Repeat("x", hubAuthBodyLimit) + `"}`
	req := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:8090")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestHubPasskeyStrictJSONContentType(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	h := s.handler()
	request := func(contentType, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/verify", strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Origin", "http://localhost:8090")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := request("application/jsonjunk", `{"code":"abcdefghijklmnopqrst"}`); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type=%d", w.Code)
	}
	if w := request("application/json", `{"code":"abcdefghijklmnopqrst","unknown":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field=%d %s", w.Code, w.Body.String())
	}
	if w := request("application/json; charset=utf-8", `{"code":"abcdefghijklmnopqrst"}`); w.Code != http.StatusOK {
		t.Fatalf("valid JSON after malformed=%d %s", w.Code, w.Body.String())
	}
}

func TestHubPasskeyBootstrapVerifyCookieSecurityAndOrigin(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	h := s.handler()
	req := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/verify", strings.NewReader(`{"code":"abcdefghijklmnopqrst"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:8090")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%v", cookies)
	}
	c := cookies[0]
	if c.Name != hubBootstrapCookieName || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure {
		t.Fatalf("cookie=%+v", c)
	}
	refresh := httptest.NewRequest(http.MethodGet, "http://backend/auth/setup", nil)
	dataRequest := httptest.NewRequest(http.MethodGet, "http://backend/api/nodes", nil)
	dataRequest.AddCookie(c)
	dataResponse := httptest.NewRecorder()
	h.ServeHTTP(dataResponse, dataRequest)
	if dataResponse.Code != http.StatusUnauthorized {
		t.Fatalf("bootstrap grant accessed Hub data: %d", dataResponse.Code)
	}
	refresh.AddCookie(c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, refresh)
	if w.Code != http.StatusOK {
		t.Fatalf("active bootstrap grant refresh=%d %s", w.Code, w.Body.String())
	}
	reuse := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/verify", strings.NewReader(`{"code":""}`))
	reuse.Header.Set("Origin", "http://localhost:8090")
	reuse.Header.Set("Content-Type", "application/json")
	reuse.AddCookie(c)
	reuseResponse := httptest.NewRecorder()
	h.ServeHTTP(reuseResponse, reuse)
	if reuseResponse.Code != http.StatusOK {
		t.Fatalf("active grant could not resume: %d %s", reuseResponse.Code, reuseResponse.Body.String())
	}
	if strings.Contains(w.Body.String(), "abcdefghijklmnopqrst") {
		t.Fatal("bootstrap secret rendered in setup page")
	}
	wantCSP := "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") != wantCSP || w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("auth page security headers=%v", w.Header())
	}
	if strings.Contains(w.Body.String(), "<style") || strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "<base") || strings.Contains(w.Body.String(), "rel=\"icon\"") {
		t.Fatalf("auth page contains inline or relative-base execution: %s", w.Body.String())
	}
	login := httptest.NewRequest(http.MethodGet, "http://backend/auth/login", nil)
	login.AddCookie(c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, login)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/auth/setup" {
		t.Fatalf("login redirect=%d %q", w.Code, w.Header().Get("Location"))
	}
	bad := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/login/begin", strings.NewReader(`{}`))
	bad.Header.Set("Content-Type", "application/json")
	bad.Header.Set("Origin", "https://evil.test")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, bad)
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong origin=%d", w.Code)
	}
}
func TestHubPasskeySessionCookieAttributes(t *testing.T) {
	s := newTestPasskeyHub(t, "/hub")
	s.passkey.endpoint.Secure = true
	issued, err := s.passkey.sessions.Create("cred")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.setSessionCookie(w, issued)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal(cookies)
	}
	c := cookies[0]
	if c.Name != hubSessionCookieName || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/hub/" || c.MaxAge != 7*24*60*60 || c.Domain != "" {
		t.Fatalf("%+v", c)
	}
	if c.Value == "" || strings.Contains(c.Value, "cred") {
		t.Fatalf("non-opaque cookie %q", c.Value)
	}
	clear := httptest.NewRecorder()
	s.clearCookie(clear, hubSessionCookieName)
	cleared := clear.Result().Cookies()[0]
	if cleared.Name != c.Name || cleared.Path != c.Path || cleared.MaxAge >= 0 || !cleared.Expires.Before(time.Now()) || cleared.Secure != c.Secure || cleared.SameSite != c.SameSite {
		t.Fatalf("clear cookie mismatch: set=%+v clear=%+v", c, cleared)
	}
}

func TestHubPasskeyConfiguredOriginBehindProxy(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	issued, err := s.passkey.sessions.Create("cred")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/logout", strings.NewReader(`{}`))
	req.Host = "internal.service:8090"
	req.Header.Set("Origin", "http://localhost:8090")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: hubSessionCookieName, Value: issued.Token})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestHubPasskeyProxyStripsAllHubCredentials(t *testing.T) {
	var gotAuth, gotCookie string
	backendCalls := 0
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte("ok"))
	})
	configureTestPasskeyHub(t, s, "")
	issued, err := s.passkey.sessions.Create("credential")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://backend/node/alpha/models", nil)
	req.Header.Set("Origin", "http://localhost:8090")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer browser-authorization")
	for _, cookie := range []*http.Cookie{{Name: hubSessionCookieName, Value: issued.Token}, {Name: hubBootstrapCookieName, Value: "bootstrap"}, {Name: hubRecoveryCookieName, Value: "recovery"}, {Name: hubLoginCeremonyCookieName, Value: "ceremony"}} {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer tkn-123" || gotCookie != "" {
		t.Fatalf("backend auth=%q cookie=%q", gotAuth, gotCookie)
	}
	assertion := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/login/finish", strings.NewReader(`{"id":"assertion"}`))
	assertion.Header.Set("Origin", "http://localhost:8090")
	assertion.Header.Set("Content-Type", "application/json")
	assertionResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(assertionResponse, assertion)
	if assertionResponse.Code != http.StatusUnauthorized || backendCalls != 1 {
		t.Fatalf("assertion route status=%d backend calls=%d", assertionResponse.Code, backendCalls)
	}
}

func TestHubPasskeySessionAndExplicitBearerCompatibility(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	issued, err := s.passkey.sessions.Create("cred")
	if err != nil {
		t.Fatal(err)
	}
	h := s.handler()
	sessionRequest := httptest.NewRequest(http.MethodGet, "http://backend/api/auth/session", nil)
	sessionRequest.AddCookie(&http.Cookie{Name: hubSessionCookieName, Value: issued.Token})
	sessionResponse := httptest.NewRecorder()
	h.ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK || !strings.Contains(sessionResponse.Body.String(), hubPasskeyUserName) || !strings.Contains(sessionResponse.Body.String(), `"active_sessions":1`) {
		t.Fatalf("session response=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	for _, secret := range []string{issued.Token, "cred", issued.Info.ID} {
		if strings.Contains(sessionResponse.Body.String(), secret) {
			t.Fatalf("session response leaked %q: %s", secret, sessionResponse.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "http://backend/api/nodes", nil)
	req.AddCookie(&http.Cookie{Name: hubSessionCookieName, Value: issued.Token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("session=%d %s", w.Code, w.Body.String())
	}
	s.token = "explicit"
	req = httptest.NewRequest(http.MethodGet, "http://backend/api/nodes", nil)
	req.Header.Set("Authorization", "Bearer explicit")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("bearer=%d", w.Code)
	}
	mutation := httptest.NewRequest(http.MethodPost, "http://backend/api/nodes/test", strings.NewReader(`{"url":"http://127.0.0.1:1/chat"}`))
	mutation.Header.Set("Authorization", "Bearer explicit")
	mutation.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, mutation)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Fatalf("explicit bearer mutation rejected without browser Origin: %d %s", w.Code, w.Body.String())
	}
	management := httptest.NewRequest(http.MethodGet, "http://backend/api/auth/credentials", nil)
	management.Header.Set("Authorization", "Bearer explicit")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, management)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer accessed browser credential management: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "http://backend/api/nodes?token=explicit", nil)
	req.AddCookie(&http.Cookie{Name: hubAuthCookieName, Value: "explicit"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("legacy/query unexpectedly accepted: %d", w.Code)
	}
	foundClear := false
	for _, c := range w.Result().Cookies() {
		if c.Name == hubAuthCookieName && c.MaxAge < 0 {
			foundClear = true
		}
	}
	if !foundClear {
		t.Fatal("legacy cookie not expired")
	}
}
func TestHubPasskeyRateLimitResponse(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	h := s.handler()
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/verify", strings.NewReader(`{"code":"wrong-wrong-wrong-wrong"}`))
		req.RemoteAddr = "192.0.2.55:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("wrong-origin request %d status=%d", i, w.Code)
		}
	}
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/verify", strings.NewReader(`{"code":"wrong-wrong-wrong-wrong"}`))
		req.RemoteAddr = "192.0.2.55:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost:8090")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if i < 5 && w.Code != http.StatusUnauthorized {
			t.Fatalf("request %d status=%d", i, w.Code)
		}
		if i == 5 && (w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || w.Header().Get("Cache-Control") != "no-store") {
			t.Fatalf("rate response status=%d headers=%v", w.Code, w.Header())
		}
	}
}

func TestHubPasskeyGlobalRateLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newHubAuthLimiter(func() time.Time { return now })
	for i := 0; i < 20; i++ {
		if !limiter.allow(fmt.Sprintf("192.0.2.%d:1", i+1)) {
			t.Fatalf("global request %d denied", i)
		}
	}
	if limiter.allow("198.51.100.1:1") {
		t.Fatal("global burst exceeded")
	}
	now = now.Add(600 * time.Millisecond)
	if !limiter.allow("198.51.100.1:1") {
		t.Fatal("global token did not refill")
	}
}

func TestHubPasskeyRateLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newHubAuthLimiter(func() time.Time { return now })
	for i := 0; i < 5; i++ {
		if !limiter.allow("192.0.2.1:1234") {
			t.Fatalf("request %d denied", i)
		}
	}
	if limiter.allow("192.0.2.1:9999") {
		t.Fatal("per-address burst exceeded")
	}
	now = now.Add(6 * time.Second)
	if !limiter.allow("192.0.2.1:1") {
		t.Fatal("token did not refill")
	}
}

func TestResolveHubBootstrapSecretNonInteractiveAndEnvScrub(t *testing.T) {
	oldTerminal, oldFile, oldPrint := hubOutputIsTerminal, serveHubBootstrapTokenFile, serveHubPrintBootstrapToken
	t.Cleanup(func() {
		hubOutputIsTerminal = oldTerminal
		serveHubBootstrapTokenFile = oldFile
		serveHubPrintBootstrapToken = oldPrint
	})
	hubOutputIsTerminal = func(any) bool { return false }
	serveHubBootstrapTokenFile = ""
	serveHubPrintBootstrapToken = false
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "")
	if _, _, err := resolveHubBootstrapSecret(serveHubCmd, true); err == nil {
		t.Fatal("expected noninteractive error")
	}
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "abcdefghijklmnopqrst")
	secret, display, err := resolveHubBootstrapSecret(serveHubCmd, true)
	if err != nil || string(secret) != "abcdefghijklmnopqrst" || display != "" {
		t.Fatalf("secret=%q display=%q err=%v", secret, display, err)
	}
	if value, ok := os.LookupEnv("TERM_LLM_HUB_BOOTSTRAP_TOKEN"); ok {
		t.Fatalf("environment not scrubbed: %q", value)
	}
	secretFile := filepath.Join(t.TempDir(), "bootstrap")
	if err := os.WriteFile(secretFile, []byte("file-secret-abcdefghijklmnop"), 0o600); err != nil {
		t.Fatal(err)
	}
	serveHubBootstrapTokenFile = secretFile
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "environment-secret-abcdef")
	secret, display, err = resolveHubBootstrapSecret(serveHubCmd, true)
	if err != nil || string(secret) != "file-secret-abcdefghijklmnop" || display != "" {
		t.Fatalf("file precedence secret=%q display=%q err=%v", secret, display, err)
	}
	if _, ok := os.LookupEnv("TERM_LLM_HUB_BOOTSTRAP_TOKEN"); ok {
		t.Fatal("environment not scrubbed when file won")
	}
	serveHubBootstrapTokenFile = ""
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "")
	serveHubPrintBootstrapToken = true
	secret, display, err = resolveHubBootstrapSecret(serveHubCmd, true)
	if err != nil || len(secret) == 0 || display == "" {
		t.Fatalf("print override secret=%q display=%q err=%v", secret, display, err)
	}
}

func TestLockHubPasskeyStateExcludesOverlappingHub(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(dir, "auth.json")
	unlock, err := lockHubPasskeyState(authFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockHubPasskeyState(authFile); err == nil {
		t.Fatal("overlapping Hub acquired passkey state lock")
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock, err = lockHubPasskeyState(authFile)
	if err != nil {
		t.Fatalf("passkey state remained locked: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestNewHubPasskeyRuntimeRequiresStateStores(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	for name, args := range map[string]struct {
		store               *passkeyauth.Store
		sessions            *passkeyauth.Sessions
		bootstrap, recovery *passkeyauth.Grants
	}{
		"store":     {nil, s.passkey.sessions, s.passkey.bootstrap, s.passkey.recovery},
		"sessions":  {s.passkey.store, nil, s.passkey.bootstrap, s.passkey.recovery},
		"bootstrap": {s.passkey.store, s.passkey.sessions, nil, s.passkey.recovery},
		"recovery":  {s.passkey.store, s.passkey.sessions, s.passkey.bootstrap, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newHubPasskeyRuntime(s.passkey.endpoint, args.store, args.sessions, args.bootstrap, args.recovery, nil); err == nil {
				t.Fatal("accepted incomplete passkey runtime state")
			}
		})
	}
}

func TestHubPasskeyBearerCompatibilitySummary(t *testing.T) {
	if got := hubPasskeyBearerCompatibilitySummary(tokenSourceFlag); got != "from --token" {
		t.Fatalf("flag summary = %q", got)
	}
	if got := hubPasskeyBearerCompatibilitySummary(tokenSourceEnv); got != "from TERM_LLM_HUB_TOKEN" {
		t.Fatalf("env summary = %q", got)
	}
	if got := hubPasskeyBearerCompatibilitySummary(tokenSourceNone); got != "" {
		t.Fatalf("disabled summary = %q", got)
	}
}

func TestResolveHubAuthMode(t *testing.T) {
	for _, mode := range []string{"bearer", "none", "passkey"} {
		if got, err := resolveHubAuthMode(mode); err != nil || got != mode {
			t.Fatalf("%s => %s %v", mode, got, err)
		}
	}
	if _, err := resolveHubAuthMode("invalid"); err == nil {
		t.Fatal("expected error")
	}
}
