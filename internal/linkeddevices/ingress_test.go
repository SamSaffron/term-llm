package linkeddevices

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrepareMountedRequestStripsExactlyOnce(t *testing.T) {
	tests := []struct {
		name, basePath, target, wantPath string
		wantErr                          bool
	}{
		{name: "root", basePath: "/", target: "/v1/sessions?limit=1", wantPath: "/v1/sessions"},
		{name: "mount root", basePath: "/chat", target: "/chat", wantPath: "/"},
		{name: "mounted API", basePath: "/chat", target: "/chat/v1/sessions?limit=1", wantPath: "/v1/sessions"},
		{name: "deep mount", basePath: "/deep/mount", target: "/deep/mount/dist/app.js", wantPath: "/dist/app.js"},
		{name: "strip once", basePath: "/chat", target: "/chat/chat/v1/sessions", wantPath: "/chat/v1/sessions"},
		{name: "neighbor", basePath: "/chat", target: "/chatty/v1/sessions", wantErr: true},
		{name: "unmounted", basePath: "/chat", target: "/v1/sessions", wantErr: true},
		{name: "encoded mount", basePath: "/chat", target: "/ch%61t/v1/sessions", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			err := prepareMountedRequest(request, test.basePath)
			if (err != nil) != test.wantErr {
				t.Fatalf("prepareMountedRequest() error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && request.URL.Path != test.wantPath {
				t.Fatalf("path = %q, want %q", request.URL.Path, test.wantPath)
			}
		})
	}
}

func TestRouteAllowedDefaultDeny(t *testing.T) {
	tests := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/", true},
		{http.MethodGet, "/chat/session-1", true},
		{http.MethodGet, "/chat/session-1/extra", false},
		{http.MethodGet, "/future-owner-route", false},
		{http.MethodGet, "/dist/app.js", true},
		{http.MethodGet, "/v1/sessions", true},
		{http.MethodPost, "/v1/sessions", true},
		{http.MethodGet, "/v1/sessions/s1/messages", true},
		{http.MethodGet, "/v1/sessions/s1/transcript", true},
		{http.MethodGet, "/v1/sessions/s1/file-changes/content", false},
		{http.MethodGet, "/v1/sessions/s1/shares", false},
		{http.MethodGet, "/v1/sessions/s1/tree", false},
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
		{http.MethodGet, "/v1/linked-devices", false},
		{http.MethodPost, "/v1/linked-devices/stop", false},
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
