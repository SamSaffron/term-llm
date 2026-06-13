package cmd

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/contain"
	"github.com/samsaffron/term-llm/internal/widgets"
)

// widgetGateway builds a gateway whose widget host is enabled and whose single
// agent "alpha" proxies to the given backend (simulating that agent's serve).
func widgetGateway(t *testing.T, basePath string, h http.HandlerFunc) *gateway {
	t.Helper()
	backend := httptest.NewServer(h)
	t.Cleanup(backend.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	g := fakeGateway(host, map[string]contain.WebConfig{
		"alpha": {Port: port, Token: "tkn-123", BasePath: basePath},
	})
	g.widgetHost = "widgets.localhost"
	return g
}

// widgetReq builds a request addressed to the widget-host origin.
func widgetReq(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Host = "widgets.localhost"
	return r
}

func TestWidgetHostProxiesToAgentWidgetSubtree(t *testing.T) {
	var gotPath, gotAuth, gotCookie, gotKey string
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotKey = r.Header.Get("X-Api-Key")
		io.WriteString(w, "widget asset")
	})

	rec := httptest.NewRecorder()
	req := widgetReq(http.MethodGet, "/w/alpha/job-usage/assets/app.js")
	// Client-supplied credentials must never reach the backend.
	req.Header.Set("Authorization", "Bearer client-should-not-pass")
	req.Header.Set("Cookie", "term_llm_token=nope")
	req.Header.Set("X-Api-Key", "client-key")
	g.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if gotPath != "/chat/widgets/job-usage/assets/app.js" {
		t.Errorf("backend path = %q want /chat/widgets/job-usage/assets/app.js", gotPath)
	}
	if gotAuth != "Bearer tkn-123" {
		t.Errorf("backend Authorization = %q want Bearer tkn-123", gotAuth)
	}
	if gotCookie != "" {
		t.Errorf("client cookie reached backend: %q", gotCookie)
	}
	if gotKey != "" {
		t.Errorf("client x-api-key reached backend: %q", gotKey)
	}
	if rec.Body.String() != "widget asset" {
		t.Errorf("body = %q want widget asset", rec.Body.String())
	}
}

func TestWidgetHostRedirectsMissingTrailingSlash(t *testing.T) {
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("backend should not be hit for a bare mount, got %q", r.URL.Path)
	})
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, widgetReq(http.MethodGet, "/w/alpha/job-usage"))

	if rec.Code != http.StatusTemporaryRedirect && rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d want a redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/w/alpha/job-usage/" {
		t.Fatalf("Location = %q want /w/alpha/job-usage/", loc)
	}
}

func TestWidgetHostRedirectPreservesQuery(t *testing.T) {
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("backend should not be hit for a bare mount, got %q", r.URL.Path)
	})
	rec := httptest.NewRecorder()
	g.handleWidgetProxy(rec, widgetReq(http.MethodGet, "/w/alpha/job-usage?foo=bar&x=1"))

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d want 307", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/w/alpha/job-usage/?foo=bar&x=1" {
		t.Fatalf("Location = %q want /w/alpha/job-usage/?foo=bar&x=1 (query must be preserved)", loc)
	}
}

func TestWidgetHostDoesNotRebaseHTML(t *testing.T) {
	// A widget that emits the agent's baked-in prefix must be passed through
	// untouched: widgets resolve against their own origin via relative URLs, and
	// the widget host must not run the SPA prefix rebasing.
	html := `<base href="/chat/"><script>window.TERM_LLM_UI_PREFIX="/chat";</script>`
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, html)
	})
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, widgetReq(http.MethodGet, "/w/alpha/job-usage/"))

	if rec.Body.String() != html {
		t.Fatalf("widget HTML was rewritten:\n got=%q\nwant=%q", rec.Body.String(), html)
	}
}

func TestWidgetHostRejectsInvalidMount(t *testing.T) {
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("backend hit with invalid mount: %q", r.URL.Path)
	})
	rec := httptest.NewRecorder()
	g.handleWidgetProxy(rec, widgetReq(http.MethodGet, "/w/alpha/Bad_Mount/"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400 for invalid mount", rec.Code)
	}
}

func TestWidgetHostRejectsEncodedTraversal(t *testing.T) {
	hit := false
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) { hit = true })
	rec := httptest.NewRecorder()
	g.handleWidgetProxy(rec, widgetReq(http.MethodGet, "/w/alpha/job-usage/..%2f..%2fv1/models"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", rec.Code)
	}
	if hit {
		t.Fatal("backend received an encoded-traversal widget request")
	}
}

func TestWidgetHostUnknownAgentIs404(t *testing.T) {
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, widgetReq(http.MethodGet, "/w/ghost/job-usage/"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404", rec.Code)
	}
}

// The widget origin must host NOTHING sensitive: the agent gateway routes and
// landing page must not be reachable there. That emptiness is the isolation.
func TestWidgetHostDoesNotExposeGatewayRoutes(t *testing.T) {
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	for _, path := range []string{"/agents", "/agent/alpha/", "/"} {
		rec := httptest.NewRecorder()
		g.handler().ServeHTTP(rec, widgetReq(http.MethodGet, path))
		if rec.Code != http.StatusNotFound {
			t.Errorf("widget host exposed %q (status %d), want 404", path, rec.Code)
		}
	}
}

// Conversely, the widget route must not be reachable on the agent gateway origin.
func TestGatewayIndexShowsWidgetOriginWhenEnabled(t *testing.T) {
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	// Landing page is on the agent origin (not the widget host).
	g.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "widgets.localhost") {
		t.Errorf("landing page does not surface the widget origin:\n%s", body)
	}
}

func TestGatewayIndexListsWidgets(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{
		"alpha": {Port: "8081", Token: "t", BasePath: "/chat"},
	})
	g.widgetHost = "widgets.localhost"
	g.fetchWidgets = func(web contain.WebConfig) ([]widgets.WidgetStatus, error) {
		return []widgets.WidgetStatus{
			{Mount: "job-usage", Title: "Job Usage", State: "running"},
			{Mount: "notes", Title: "Notes", State: "running"},
		}, nil
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:8095"
	g.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Job Usage") || !strings.Contains(body, "Notes") {
		t.Errorf("widget titles missing from page:\n%s", body)
	}
	// Links must target the widget origin (same port, widget hostname).
	if !strings.Contains(body, "http://widgets.localhost:8095/w/alpha/job-usage/") {
		t.Errorf("widget link not pointed at the widget origin:\n%s", body)
	}
}

func TestGatewayIndexGroupsWidgetsByAgent(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{
		"alpha": {Port: "8081", Token: "t", BasePath: "/chat"},
		"beta":  {Port: "8082", Token: "t", BasePath: "/chat"},
	})
	g.widgetHost = "widgets.localhost"
	g.fetchWidgets = func(web contain.WebConfig) ([]widgets.WidgetStatus, error) {
		if web.Port == "8081" {
			return []widgets.WidgetStatus{{Mount: "notes", Title: "Notes", State: "running"}}, nil
		}
		return []widgets.WidgetStatus{{Mount: "usage", Title: "Usage", State: "stopped"}}, nil
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:8095"
	g.handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, `class="wgroup"`) {
		t.Fatalf("expected a grouped Widgets section:\n%s", body)
	}
	// Widgets must live in the Widgets section, not leak into the agents list.
	agentsUL := body[strings.Index(body, `<ul class="agents">`):strings.Index(body, "</ul>")]
	if strings.Contains(agentsUL, "wpill") {
		t.Errorf("widget pills leaked into the agents list:\n%s", agentsUL)
	}
	// Each agent's widget resolves under its own agent on the widget origin.
	if !strings.Contains(body, "/w/alpha/notes/") || !strings.Contains(body, "/w/beta/usage/") {
		t.Errorf("per-agent widget links missing:\n%s", body)
	}
}

func TestGatewayIndexWidgetFetchErrorIsSoft(t *testing.T) {
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{
		"alpha": {Port: "8081", Token: "t", BasePath: "/chat"},
	})
	g.widgetHost = "widgets.localhost"
	g.fetchWidgets = func(web contain.WebConfig) ([]widgets.WidgetStatus, error) {
		return nil, fmt.Errorf("agent down")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:8095"
	g.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (widget fetch failure must not break the page)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "alpha") {
		t.Errorf("agent still expected on page despite widget fetch error")
	}
	if strings.Contains(rec.Body.String(), "/w/alpha/") {
		t.Errorf("no widget links expected when the fetch failed")
	}
}

func TestGatewayIndexOmitsWidgetSectionWhenDisabled(t *testing.T) {
	// Widget host disabled: no widget origin reference on the landing page.
	g := fakeGateway("127.0.0.1", map[string]contain.WebConfig{
		"alpha": {Port: "8081", Token: "t", BasePath: "/chat"},
	})
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "/w/") {
		t.Errorf("widget section rendered while widget host disabled:\n%s", rec.Body.String())
	}
}

func TestAgentHostDoesNotExposeWidgetRoute(t *testing.T) {
	g := widgetGateway(t, "/chat", func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	// Default origin (not the widget host).
	req := httptest.NewRequest(http.MethodGet, "/w/alpha/job-usage/", nil)
	g.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("agent origin exposed /w/ route (status %d), want 404", rec.Code)
	}
}
