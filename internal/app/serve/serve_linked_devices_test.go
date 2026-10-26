package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/linkeddevices"
)

func TestPeerBootstrapAndHealthExposeScopedReadOnlyIdentity(t *testing.T) {
	server := newTestServeServer()
	defer server.sessionMgr.Close()
	server.cfg.ui = true
	server.cfg.basePath = "/chat"
	server.cfg.requireAuth = true
	server.cfg.token = "owner-token"
	server.cfg.agentName = "agent"
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = linkeddevices.WithDevicePrincipal(request, linkeddevices.DevicePrincipal{DeviceID: "device", Scope: linkeddevices.ScopeWebV1})
	for _, path := range []string{"/", "/index.html", "/session-1"} {
		peerRequest := request.Clone(request.Context())
		peerRequest.URL.Path = path
		// Exercise both explicit index and SPA fallback through the shared shell handler.
		recorder := httptest.NewRecorder()
		server.handleUI(recorder, peerRequest)
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK || !strings.Contains(body, `TERM_LLM_AUTH_MODE="linked-devices"`) || !strings.Contains(body, `TERM_LLM_LINKED_DEVICES_SCOPE="web-v1"`) {
			t.Fatalf("%s: peer bootstrap marker missing: %d %s", path, recorder.Code, body)
		}
	}
	owner := httptest.NewRecorder()
	server.handleUI(owner, httptest.NewRequest(http.MethodGet, "/", nil))
	ownerBody := owner.Body.String()
	if strings.Contains(ownerBody, `TERM_LLM_LINKED_DEVICES_SCOPE`) {
		t.Fatal("cached owner shell inherited peer bootstrap marker")
	}
	recorder := httptest.NewRecorder()
	server.handleHealth(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"agent":"agent"`) {
		t.Fatalf("peer health = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestPasskeyHealthIdentityRejectsBearerButTrustsLinkedDevice(t *testing.T) {
	server := newTestServeServer()
	defer server.sessionMgr.Close()
	server.cfg.requireAuth = true
	server.cfg.token = "legacy-owner-token"
	server.cfg.agentName = "agent"
	server.browserAuth = newWebPasskeyHandler(newTestPasskeyHub(t, "/chat").passkey)

	bearer := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	bearer.Header.Set("Authorization", "Bearer legacy-owner-token")
	recorder := httptest.NewRecorder()
	server.handleHealth(recorder, bearer)
	if strings.Contains(recorder.Body.String(), `"agent"`) {
		t.Fatalf("passkey-mode bearer exposed identity: %s", recorder.Body.String())
	}

	device := linkeddevices.WithDevicePrincipal(bearer, linkeddevices.DevicePrincipal{DeviceID: "device", Scope: linkeddevices.ScopeWebV1})
	recorder = httptest.NewRecorder()
	server.handleHealth(recorder, device)
	if !strings.Contains(recorder.Body.String(), `"agent":"agent"`) {
		t.Fatalf("linked-device health omitted identity in passkey mode: %s", recorder.Body.String())
	}
}

func TestPeerCapabilitiesDisableOwnerAndDeviceOnlyControls(t *testing.T) {
	server := newTestServeServer()
	defer server.sessionMgr.Close()
	server.cfg.ui = true
	request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	request = linkeddevices.WithDevicePrincipal(request, linkeddevices.DevicePrincipal{DeviceID: "device", Scope: linkeddevices.ScopeWebV1})
	recorder := httptest.NewRecorder()
	server.handleCapabilities(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	linked := payload["linked_devices"].(map[string]any)
	if linked["active"] != true || linked["scope"] != linkeddevices.ScopeWebV1 || linked["owner_controls"] != false ||
		linked["uploads"] != false || linked["voice"] != false || linked["push_notifications"] != false || linked["device_tools"] != false {
		t.Fatalf("linked-device capabilities = %#v", linked)
	}
	for _, feature := range []string{"projects", "worktrees", "shell", "live"} {
		value, _ := payload[feature].(map[string]any)
		if value["enabled"] != false {
			t.Errorf("%s capability = %#v, want disabled", feature, value)
		}
	}
}

func TestServeAuthAdmitsOnlyVerifiedMobilePrincipal(t *testing.T) {
	server := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "owner-token"}}
	handler := server.auth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	ordinary := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	recorder := httptest.NewRecorder()
	handler(recorder, ordinary)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("ordinary unauthenticated request = %d", recorder.Code)
	}

	device := linkeddevices.WithDevicePrincipal(ordinary, linkeddevices.DevicePrincipal{DeviceID: "device", Scope: linkeddevices.ScopeWebV1})
	recorder = httptest.NewRecorder()
	handler(recorder, device)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("verified mobile principal = %d", recorder.Code)
	}

	wrongScope := linkeddevices.WithDevicePrincipal(ordinary, linkeddevices.DevicePrincipal{DeviceID: "device", Scope: "owner"})
	recorder = httptest.NewRecorder()
	handler(recorder, wrongScope)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-scope principal = %d", recorder.Code)
	}
}
