package linkeddevices

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/peer"
)

func TestLinkedLiveRouteAllowlist(t *testing.T) {
	root := "/v1/live/sessions"
	id := root + "/live_20261027-123456-abcd"
	allowed := map[string]string{
		root: http.MethodPost, id: http.MethodDelete,
		id + "/events": http.MethodGet, id + "/audio/output": http.MethodGet,
		id + "/audio/input": http.MethodPost, id + "/diagnostics": http.MethodPost,
		id + "/text": http.MethodPost, id + "/session": http.MethodPost,
	}
	for path, wantMethod := range allowed {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch, http.MethodOptions} {
			if got := RouteAllowed(method, path); got != (method == wantMethod) {
				t.Errorf("RouteAllowed(%q, %q) = %v", method, path, got)
			}
		}
	}
	denied := []string{
		root + "/", root + "//events", root + "/live_/events", root + "/other/events",
		root + "/live_../events", root + "/live_x%2fy/events", root + "/live_x%20/events",
		root + "/live_x.y/events", root + "/live_x y/events", root + "/live_é/events",
		root + "/" + strings.Repeat("x", 129) + "/events",
		id + "/", id + "/events/", id + "//events", id + "/extra/events",
		id + "/audio", id + "/audio/", id + "/audio/input/extra", id + "/audio/output/extra",
		id + "/client_tools", id + "/tool_calls/call_1/result", id + "/delegations/d1/result",
		id + "/text/extra", id + "/session/extra", id + "/diagnostics/extra", "/v1/lives/sessions",
	}
	for _, path := range denied {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete} {
			if RouteAllowed(method, path) {
				t.Errorf("unexpected Live route: %s %s", method, path)
			}
		}
	}
}

func TestShapeCapabilitiesPreservesActualLinkedLive(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, transport := range []string{"http_pcm", "webrtc"} {
			t.Run(transport+"/"+map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
				original := map[string]any{
					"enabled": enabled, "provider": "real-provider", "model": "real-model",
					"transport": transport, "version": 3, "reason": "host reason",
					"delegation_modes": []string{"server", "client"},
				}
				payload := map[string]any{"live": original}
				request := WithDevicePrincipal(httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil), DevicePrincipal{DeviceID: "d1", Scope: ScopeWebV1})
				response := httptest.NewRecorder()
				ShapeCapabilities(response, request, payload)
				live := payload["live"].(map[string]any)
				for _, key := range []string{"enabled", "provider", "model", "transport", "version", "reason"} {
					if live[key] != original[key] {
						t.Errorf("%s = %v, want %v", key, live[key], original[key])
					}
				}
				if !reflect.DeepEqual(live["delegation_modes"], []string{"server"}) || !reflect.DeepEqual(original["delegation_modes"], []string{"server", "client"}) {
					t.Fatalf("delegation modes = %v; original = %v", live, original)
				}
				linked := payload["linked_devices"].(map[string]any)
				if linked["voice"] != enabled || linked["device_tools"] != false || linked["uploads"] != false || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("linked capabilities = %v, headers = %v", linked, response.Header())
				}
			})
		}
	}
}

func TestPeerRemovalCancelsOnlyMatchingLifetime(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpoint, stale := &peer.Endpoint{}, &peer.Endpoint{}
	runtime := &Runtime{sessions: map[string]*deviceSession{"device": {endpoint: endpoint, cancel: cancel}}}
	runtime.removeSession("device", stale)
	if lifetime.Err() != nil || runtime.sessions["device"] == nil {
		t.Fatal("stale peer removal ended the replacement connection")
	}
	runtime.removeSession("device", endpoint)
	if lifetime.Err() == nil || runtime.sessions["device"] != nil {
		t.Fatal("peer removal did not end its trusted lifetime")
	}
}
