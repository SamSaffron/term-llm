package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Widget forms/fetches need cookie authentication, unlike ordinary API writes.
// A different port (or sibling host) is a different origin but can still send
// SameSite=Strict cookies. Model a widget mutation without running local code.
func TestWidgetCookieMutationBoundary(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			for _, tc := range []struct {
				name, origin, site, auth, apiKey, publicURL, hubURL string
				cors                                                []string
				want                                                bool
			}{
				{name: "same origin", origin: "http://localhost:8080", site: "same-origin", want: true},
				{name: "same site different port", origin: "http://localhost:8997", site: "same-site"},
				{name: "same site without metadata", origin: "http://localhost:8997"},
				{name: "cross site", origin: "https://untrusted.example", site: "cross-site"},
				{name: "metadata only same site", site: "same-site"},
				{name: "same origin without metadata", origin: "http://localhost:8080", want: true},
				{name: "non browser", want: true},
				{name: "configured public URL", origin: "https://chat.example", publicURL: "https://chat.example/ui", want: true},
				{name: "configured hub URL", origin: "https://hub.example", hubURL: "https://hub.example/hub", want: true},
				{name: "hub metadata", origin: "https://hub.example", site: "same-origin", want: true},
				{name: "explicit origin", origin: "https://app.example", site: "cross-site", cors: []string{"https://app.example"}, want: true},
				{name: "explicit wildcard", origin: "https://app.example", site: "cross-site", cors: []string{"*"}, want: true},
				{name: "explicit bearer", origin: "http://localhost:8997", site: "same-site", auth: "Bearer synthetic-token", want: true},
				{name: "explicit API key", origin: "http://localhost:8997", site: "same-site", apiKey: "synthetic-token", want: true},
				{name: "invalid bearer cannot fall back", origin: "http://localhost:8080", site: "same-origin", auth: "Bearer wrong"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					srv := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "synthetic-token", corsOrigins: tc.cors, publicURL: tc.publicURL, hubURL: tc.hubURL}}
					mutations := 0
					h := srv.auth(func(w http.ResponseWriter, r *http.Request) { mutations++; w.WriteHeader(http.StatusNoContent) })
					r := httptest.NewRequest(method, "http://localhost:8080/widgets/example/action", strings.NewReader("action=synthetic"))
					r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					r.Header.Set("Origin", tc.origin)
					r.Header.Set("Sec-Fetch-Site", tc.site)
					r.Header.Set("Authorization", tc.auth)
					r.Header.Set("X-Api-Key", tc.apiKey)
					// Forwarded headers must not create an allowed origin.
					r.Header.Set("X-Forwarded-Host", "localhost:8997")
					r.Header.Set("X-Forwarded-Proto", "http")
					r.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "synthetic-token"})
					w := httptest.NewRecorder()
					h(w, r)
					wantStatus := http.StatusUnauthorized
					if tc.want {
						wantStatus = http.StatusNoContent
					}
					if (mutations == 1) != tc.want || w.Code != wantStatus {
						t.Fatalf("mutation allowed=%v: count=%d status=%d want=%d body=%s", tc.want, mutations, w.Code, wantStatus, w.Body.String())
					}
				})
			}
		})
	}
}

func TestWidgetCookieRouteBoundary(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "synthetic-token"}}
	mux := http.NewServeMux()
	srv.registerWidgetRoutes(mux)
	for method, want := range map[string]int{http.MethodGet: http.StatusTemporaryRedirect, http.MethodPost: http.StatusUnauthorized} {
		r := httptest.NewRequest(method, "http://localhost:8080/widgets/example", nil)
		r.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "synthetic-token"})
		r.Header.Set("Origin", "http://localhost:8997")
		r.Header.Set("Sec-Fetch-Site", "same-site")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s registered widget route status=%d want=%d", method, w.Code, want)
		}
	}
	// Ordinary API writes must never gain cookie authentication.
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/sessions", nil)
	r.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "synthetic-token"})
	w := httptest.NewRecorder()
	srv.auth(func(http.ResponseWriter, *http.Request) { t.Fatal("ordinary API accepted cookie write") })(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("API status=%d", w.Code)
	}
}
