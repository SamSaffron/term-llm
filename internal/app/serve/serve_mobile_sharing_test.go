package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/mobileshare"
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
	request = mobileshare.WithDevicePrincipal(request, mobileshare.DevicePrincipal{DeviceID: "device", Scope: mobileshare.ScopeWebV1})
	body := string(mobileshare.PeerIndexHTML(request, server.renderIndexHTML()))
	if !strings.Contains(body, `TERM_LLM_AUTH_MODE="peer"`) || !strings.Contains(body, `TERM_LLM_PEER_SCOPE="web-v1"`) {
		t.Fatalf("peer bootstrap marker missing: %s", body)
	}
	ownerBody := string(mobileshare.PeerIndexHTML(httptest.NewRequest(http.MethodGet, "/", nil), server.renderIndexHTML()))
	if strings.Contains(ownerBody, `TERM_LLM_PEER_SCOPE`) {
		t.Fatal("cached owner shell inherited peer bootstrap marker")
	}
	recorder := httptest.NewRecorder()
	server.handleHealth(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"agent":"agent"`) {
		t.Fatalf("peer health = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestPeerCapabilitiesDisableOwnerAndDeviceOnlyControls(t *testing.T) {
	server := newTestServeServer()
	defer server.sessionMgr.Close()
	server.cfg.ui = true
	request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	request = mobileshare.WithDevicePrincipal(request, mobileshare.DevicePrincipal{DeviceID: "device", Scope: mobileshare.ScopeWebV1})
	recorder := httptest.NewRecorder()
	server.handleCapabilities(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	peer, _ := payload["peer"].(map[string]any)
	if peer["active"] != true || peer["scope"] != mobileshare.ScopeWebV1 || peer["owner_controls"] != false ||
		peer["uploads"] != false || peer["voice"] != false || peer["push_notifications"] != false || peer["device_tools"] != false {
		t.Fatalf("peer capabilities = %#v", peer)
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
