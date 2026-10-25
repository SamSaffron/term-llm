package serve

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samsaffron/term-llm/internal/mobileshare"
)

func TestServeAuthAdmitsOnlyVerifiedMobilePrincipal(t *testing.T) {
	server := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "owner-token"}}
	handler := server.auth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	ordinary := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	recorder := httptest.NewRecorder()
	handler(recorder, ordinary)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("ordinary unauthenticated request = %d", recorder.Code)
	}

	device := mobileshare.WithDevicePrincipal(ordinary, mobileshare.DevicePrincipal{DeviceID: "device", Scope: mobileshare.ScopeWebV1})
	recorder = httptest.NewRecorder()
	handler(recorder, device)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("verified mobile principal = %d", recorder.Code)
	}

	wrongScope := mobileshare.WithDevicePrincipal(ordinary, mobileshare.DevicePrincipal{DeviceID: "device", Scope: "owner"})
	recorder = httptest.NewRecorder()
	handler(recorder, wrongScope)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-scope principal = %d", recorder.Code)
	}
}
