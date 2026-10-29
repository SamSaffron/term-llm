package serve

import (
	"net/http"
	"net/http/httptest"
	"testing"

	appserver "github.com/samsaffron/term-llm/internal/app/serve/server"
)

func TestServeRejectsDNSRebindingHost(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		s := &serveServer{cfg: serveServerConfig{host: "127.0.0.1", basePath: "/ui", requireAuth: authenticated, token: "secret", ui: true}}
		h := appserver.GuardHost(s.httpHandler(), s.cfg.host, s.cfg.publicURL)
		for _, path := range []string{"/ui/healthz", "/"} {
			r := httptest.NewRequest(http.MethodGet, "http://attacker.example:8080"+path, nil)
			r.Header.Set("Origin", "http://attacker.example:8080")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("Authorization", "Bearer secret")
			r.Header.Set("X-Forwarded-Host", "localhost:8080")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("auth=%v path=%s status=%d; want forbidden for rebound Host", authenticated, path, w.Code)
			}
		}
	}
}
