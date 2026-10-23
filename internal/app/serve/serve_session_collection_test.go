package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionCollectionWithoutStore(t *testing.T) {
	for _, prefix := range []string{"/ui", "/chat"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, authorized := range []bool{false, true} {
				name := prefix + "/" + method
				if authorized {
					name += "/authorized"
				}
				t.Run(name, func(t *testing.T) {
					s := &serveServer{cfg: serveServerConfig{
						ui: true, basePath: prefix, requireAuth: true, token: "test-token",
					}}
					r := httptest.NewRequest(method, prefix+"/v1/sessions?limit=30", nil)
					wantStatus := http.StatusUnauthorized
					if authorized {
						r.Header.Set("Authorization", "Bearer test-token")
						wantStatus = http.StatusServiceUnavailable
					}
					w := httptest.NewRecorder()
					s.httpHandler().ServeHTTP(w, r)
					if w.Code != wantStatus {
						t.Fatalf("status = %d, want %d; body: %s", w.Code, wantStatus, w.Body.String())
					}
					if location := w.Header().Get("Location"); location != "" {
						t.Fatalf("unexpected redirect to %q", location)
					}
					if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || !json.Valid(w.Body.Bytes()) {
						t.Fatalf("expected JSON error, got %s", w.Body.String())
					}
					if authorized && !strings.Contains(w.Body.String(), "Session persistence is unavailable") {
						t.Fatalf("missing actionable persistence error: %s", w.Body.String())
					}
				})
			}
		}
	}
}
