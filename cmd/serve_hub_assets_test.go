package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHubPublicAssetsAreExactAndHardened(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	s.requireAuth = true
	s.authMode = "bearer"
	s.token = "hub-secret"
	handler := s.handler()

	for _, test := range []struct {
		path        string
		contentType string
		cache       string
	}{
		{path: "/dist/hub.js", contentType: "text/javascript; charset=utf-8", cache: "no-cache"},
		{path: "/dist/hub.css", contentType: "text/css; charset=utf-8", cache: "no-cache"},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
		if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
			t.Fatalf("GET %s status=%d bytes=%d", test.path, recorder.Code, recorder.Body.Len())
		}
		if got := recorder.Header().Get("Content-Type"); got != test.contentType {
			t.Errorf("GET %s content-type=%q, want %q", test.path, got, test.contentType)
		}
		if got := recorder.Header().Get("Cache-Control"); got != test.cache {
			t.Errorf("GET %s cache=%q, want %q", test.path, got, test.cache)
		}
		if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s nosniff=%q", test.path, got)
		}
		head := httptest.NewRecorder()
		handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, test.path, nil))
		if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
			t.Errorf("HEAD %s status=%d bytes=%d length=%q", test.path, head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
		}
		post := httptest.NewRecorder()
		handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, test.path, nil))
		if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("POST %s status=%d allow=%q", test.path, post.Code, post.Header().Get("Allow"))
		}
	}

	for _, path := range []string{"/dist/app.js", "/dist/hub.js.map", "/dist/hub.css/extra", "/api/nodes", "/"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s status=%d, want 401", path, recorder.Code)
		}
	}

	for _, path := range []string{
		"/dist/../dist/hub.js",
		"/dist/%2e%2e/dist/hub.js",
		"//dist/hub.js",
		"/dist//hub.js",
		"/dist/hub.css/../hub.css",
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer hub-secret")
		handler.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusOK {
			t.Errorf("authenticated traversal GET %s unexpectedly served a Hub asset", path)
		}
	}
}

func TestHubShellEscapesHostilePresentationConfiguration(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	const hostile = `"><script>alert("hub")</script>`
	recorder := httptest.NewRecorder()
	s.writeHubShell(recorder, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, hostile, hubPageConfig{
		Page:       "passkey-auth",
		AuthMode:   "passkey",
		BasePath:   "/hub",
		FormAction: "/hub/",
		Passkey: &hubPasskeyPageConfig{
			Mode:        "login",
			Title:       hostile,
			Heading:     hostile,
			Description: hostile,
			Button:      hostile,
		},
	})
	body := recorder.Body.String()
	if strings.Contains(body, `<script>alert`) || strings.Contains(body, `"><script`) {
		t.Fatalf("hostile presentation value escaped its configuration context: %s", body)
	}
	if !strings.Contains(body, `\u003cscript\u003e`) || !strings.Contains(body, `&lt;script&gt;`) {
		t.Fatalf("shell does not show contextual JSON and title escaping: %s", body)
	}
	for _, forbidden := range []string{"hub-secret", "registration-secret", "node-secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("shell unexpectedly contains secret marker %q", forbidden)
		}
	}
}

func TestHubCSSVersionedCacheScope(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	handler := s.handler()

	shell := httptest.NewRecorder()
	handler.ServeHTTP(shell, httptest.NewRequest(http.MethodGet, "/", nil))
	body := shell.Body.String()
	start := strings.Index(body, `/dist/hub.css?v=`)
	if start < 0 {
		t.Fatalf("shell missing versioned Hub CSS: %s", body)
	}
	versioned := body[start:]
	versioned = versioned[:strings.Index(versioned, `"`)]

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, versioned, nil))
	if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned CSS cache=%q", got)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dist/hub.css?v=wrong", nil))
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("wrong-version CSS cache=%q", got)
	}
}

func TestHubShellSecurityPoliciesAndDeepBearerAssets(t *testing.T) {
	dashboard := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	dashboardRecorder := httptest.NewRecorder()
	dashboard.handler().ServeHTTP(dashboardRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := dashboardRecorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("dashboard Cache-Control=%q", got)
	}
	if got := dashboardRecorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("dashboard nosniff=%q", got)
	}
	dashboardCSP := dashboardRecorder.Header().Get("Content-Security-Policy")
	wantDashboardCSP := "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; img-src 'self' data: http: https:"
	if dashboardCSP != wantDashboardCSP {
		t.Errorf("dashboard CSP=%q, want %q", dashboardCSP, wantDashboardCSP)
	}
	if got := dashboardRecorder.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("dashboard X-Frame-Options=%q", got)
	}
	if got := dashboardRecorder.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("dashboard Referrer-Policy=%q", got)
	}

	bearer := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	bearer.basePath = "/hub"
	bearer.requireAuth = true
	bearer.authMode = "bearer"
	bearer.token = "secret"
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/hub/node/alpha/chat/", nil)
	request.Header.Set("Accept", "text/html")
	bearer.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("deep bearer status=%d", recorder.Code)
	}
	for _, want := range []string{
		`href="/hub/dist/hub.css?v=`,
		`src="/hub/dist/hub.js"`,
		`action="/hub/node/alpha/chat/"`,
		`&#34;formAction&#34;:&#34;/hub/node/alpha/chat/&#34;`,
	} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("deep bearer shell missing %q: %s", want, recorder.Body.String())
		}
	}
	if strings.Contains(recorder.Body.String(), "<base") {
		t.Fatal("Hub shell must not depend on a base element")
	}
	for header, want := range map[string]string{
		"Cache-Control":           "no-store",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
	} {
		if got := recorder.Header().Get(header); got != want {
			t.Errorf("bearer shell %s=%q, want %q", header, got, want)
		}
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Fatal("bearer shell exposed the configured Hub token")
	}

	head := httptest.NewRecorder()
	headRequest := httptest.NewRequest(http.MethodHead, "/hub/node/alpha/chat/", nil)
	headRequest.Header.Set("Accept", "text/html")
	bearer.handler().ServeHTTP(head, headRequest)
	if head.Code != http.StatusUnauthorized || head.Body.Len() != 0 {
		t.Errorf("bearer HEAD status=%d bytes=%d", head.Code, head.Body.Len())
	}
}
