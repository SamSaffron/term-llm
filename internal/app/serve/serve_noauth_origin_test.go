package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appserver "github.com/samsaffron/term-llm/internal/app/serve/server"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/sidequestion"
)

func TestNoAuthSideQuestionBrowserBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, origin, site string
		cors               []string
		want               int
	}{
		{"cross-site simple POST", "https://untrusted.example", "cross-site", nil, http.StatusForbidden},
		{"legacy browser cross-origin POST", "https://untrusted.example", "", nil, http.StatusForbidden},
		{"opaque origin", "null", "cross-site", nil, http.StatusForbidden},
		{"same-site is not same-origin", "http://localhost:9999", "same-site", nil, http.StatusForbidden},
		{"metadata without origin", "", "cross-site", nil, http.StatusForbidden},
		{"same-origin UI", "http://localhost:8080", "same-origin", nil, http.StatusOK},
		{"legacy same-origin UI", "http://localhost:8080", "", nil, http.StatusOK},
		{"local API client", "", "", nil, http.StatusOK},
		{"Hub-proxied same-origin UI", "https://hub.example", "same-origin", nil, http.StatusOK},
		{"explicit CORS origin", "https://client.example", "cross-site", []string{"https://client.example"}, http.StatusOK},
		{"CORS does not allow other origins", "https://untrusted.example", "cross-site", []string{"https://client.example"}, http.StatusForbidden},
		{"explicit wildcard CORS", "https://client.example", "cross-site", []string{"*"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := mockprovider.NewMockProvider("mock").AddTextResponse("synthetic answer")
			manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
				rt := &serveRuntime{providerKey: "mock", defaultModel: "m"}
				rt.refreshSideQuestionSnapshot(sidequestion.Anchor{Messages: []core.Message{core.UserText("synthetic context")}})
				rt.sideProviderFactory = func(_, _ string) (core.Provider, error) { return provider, nil }
				return rt, nil
			})
			defer manager.Close()
			if _, err := manager.GetOrCreate(context.Background(), "main"); err != nil {
				t.Fatal(err)
			}
			s := &serveServer{sessionMgr: manager, cfg: serveServerConfig{
				host: "127.0.0.1", basePath: "/ui", corsOrigins: tc.cors,
			}}
			h := appserver.GuardHost(s.httpHandler(), s.cfg.host, s.cfg.publicURL)
			// text/plain is browser-safelisted: no CORS preflight or cookies are
			// needed to send this request, even when the response is unreadable.
			r := httptest.NewRequest(http.MethodPost, "http://localhost:8080/ui/api/sessions/main/side-question", strings.NewReader(`{"question":"synthetic question"}`))
			r.Header.Set("Content-Type", "text/plain")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d; body = %s", w.Code, tc.want, w.Body.String())
			}
			wantRequests := 0
			if tc.want == http.StatusOK {
				wantRequests = 1
			}
			if len(provider.Requests) != wantRequests {
				t.Errorf("provider requests = %d, want %d", len(provider.Requests), wantRequests)
			}
		})
	}
}

func TestNoAuthProjectBrowserBoundary(t *testing.T) {
	for _, crossSite := range []bool{true, false} {
		t.Run(map[bool]string{true: "cross-site", false: "same-origin"}[crossSite], func(t *testing.T) {
			s, store := newServeProjectTestServer(t)
			dir := t.TempDir()
			body, err := json.Marshal(projectCreateRequest{Path: dir, Name: "synthetic project"})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/projects", strings.NewReader(string(body)))
			r.Header.Set("Content-Type", "text/plain")
			if crossSite {
				r.Header.Set("Origin", "https://untrusted.example")
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			} else {
				r.Header.Set("Origin", "http://localhost:8080")
				r.Header.Set("Sec-Fetch-Site", "same-origin")
			}
			w := httptest.NewRecorder()
			s.httpHandler().ServeHTTP(w, r)
			want := http.StatusCreated
			if crossSite {
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Errorf("status = %d, want %d; body = %s", w.Code, want, w.Body.String())
			}
			resolved, err := resolveProjectPath(dir)
			if err != nil {
				t.Fatal(err)
			}
			project, err := store.GetProjectByCanonicalDir(context.Background(), resolved.CanonicalDir)
			if err != nil {
				t.Fatal(err)
			}
			if (project == nil) != crossSite {
				t.Errorf("project persisted = %v, cross-site = %v", project != nil, crossSite)
			}
		})
	}
}
