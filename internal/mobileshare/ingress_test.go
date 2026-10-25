package mobileshare

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouteAllowedDefaultDeny(t *testing.T) {
	tests := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/", true},
		{http.MethodGet, "/dist/app.js", true},
		{http.MethodGet, "/v1/sessions", true},
		{http.MethodPost, "/v1/sessions", true},
		{http.MethodGet, "/v1/sessions/s1/messages", true},
		{http.MethodPost, "/v1/sessions/s1/approval", true},
		{http.MethodPost, "/v1/sessions/s1/ask_user", true},
		{http.MethodPost, "/v1/responses", true},
		{http.MethodGet, "/v1/responses/r1/events", true},
		{http.MethodPost, "/v1/responses/r1/cancel", true},
		{http.MethodGet, "/files/result.png", true},
		{http.MethodDelete, "/v1/sessions/s1", false},
		{http.MethodPatch, "/v1/sessions/s1", false},
		{http.MethodGet, "/v1/sessions/s1/shell/stream", false},
		{http.MethodGet, "/v1/sessions/s1/mcp", false},
		{http.MethodPost, "/uploads", false},
		{http.MethodGet, "/admin/widgets/status", false},
		{http.MethodGet, "/v1/mobile-sharing", false},
		{http.MethodPost, "/v1/mobile-sharing/stop", false},
		{http.MethodGet, "/v1/mcp/catalogue", false},
		{http.MethodGet, "/v2/jobs", false},
		{http.MethodGet, "/api/sessions/s1", false},
		{http.MethodGet, "/auth/security", false},
		{http.MethodGet, "/extensions/example/main.js", false},
		{http.MethodGet, "/unknown.json", false},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			if got := RouteAllowed(test.method, test.path); got != test.want {
				t.Fatalf("RouteAllowed(%q, %q) = %v, want %v", test.method, test.path, got, test.want)
			}
		})
	}
}

func TestAdmitDevice(t *testing.T) {
	ownerCalls := 0
	owner := func(w http.ResponseWriter, _ *http.Request) { ownerCalls++; w.WriteHeader(http.StatusUnauthorized) }
	next := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	handler := AdmitDevice(next, owner)

	request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusUnauthorized || ownerCalls != 1 {
		t.Fatalf("ordinary request = %d, owner calls %d", recorder.Code, ownerCalls)
	}

	request = WithDevicePrincipal(request, DevicePrincipal{DeviceID: "dev_1", Scope: ScopeWebV1})
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusNoContent || ownerCalls != 1 {
		t.Fatalf("device request = %d, owner calls %d", recorder.Code, ownerCalls)
	}
}
