package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowserMutationAllowed(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			r := httptest.NewRequest(method, "http://localhost:8080/", nil)
			r.Header.Set("Origin", "https://untrusted.example")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			allowed := browserMutationAllowed(r, nil, "")
			wantAllowed := method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
			if allowed != wantAllowed {
				t.Errorf("allowed = %v, want %v", allowed, wantAllowed)
			}
		})
	}
	for _, tc := range []struct {
		name, target, origin, publicURL, forwarded string
		want                                       bool
	}{
		{"TLS origin without metadata", "https://localhost:8080/", "https://localhost:8080", "", "", true},
		{"scheme mismatch", "http://localhost:8080/", "https://localhost:8080", "", "", false},
		{"configured proxy origin", "http://localhost:8080/", "https://chat.example", "https://chat.example/ui/", "", true},
		{"untrusted forwarded scheme", "http://localhost:8080/", "https://localhost:8080", "", "https", false},
		{"untrusted forwarded host", "http://localhost:8080/", "https://untrusted.example", "", "https", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.target, nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-Forwarded-Proto", tc.forwarded)
			r.Header.Set("X-Forwarded-Host", "untrusted.example")
			if got := browserMutationAllowed(r, nil, tc.publicURL); got != tc.want {
				t.Errorf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGuardNoAuthBrowserMode(t *testing.T) {
	for _, noAuth := range []bool{false, true} {
		called := false
		h := GuardNoAuthBrowser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }), noAuth, nil, "")
		r := httptest.NewRequest(http.MethodPost, "http://localhost:8080/", nil)
		r.Header.Set("Origin", "https://untrusted.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if called == noAuth {
			t.Errorf("noAuth=%v: handler called=%v", noAuth, called)
		}
		if noAuth && w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
	}
}
