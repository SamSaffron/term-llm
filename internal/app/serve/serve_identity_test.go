package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/app/serve/authorship"
	"github.com/term-llm/term-llm/internal/linkeddevices"
	"github.com/term-llm/term-llm/internal/pairing"
	"github.com/term-llm/term-llm/internal/passkeyauth"
	"github.com/term-llm/term-llm/internal/session"
)

func TestRequestIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, id, display, email    string
		enabled                     bool
		device                      *linkeddevices.DevicePrincipal
		wantID, wantName, wantEmail string
	}{
		{name: "disabled", id: "alice"},
		{name: "header", enabled: true, id: " alice ", wantID: "alice", wantName: "alice"},
		{name: "missing", enabled: true},
		{name: "invalid ID", enabled: true, id: "a\tb"},
		{name: "oversize ID", enabled: true, id: strings.Repeat("a", 257)},
		{name: "invalid UTF8", enabled: true, id: string([]byte{255})},
		{name: "encoded", enabled: true, id: "alice", display: "=?UTF-8?B?UHJpeWEgTmF0YXJhamFu?=", email: "=?UTF-8?Q?alice=40example.com?=", wantID: "alice", wantName: "Priya Natarajan", wantEmail: "alice@example.com"},
		{name: "invalid name", enabled: true, id: "alice", display: "bad\nname", email: "bad\temail", wantID: "alice", wantName: "alice"},
		{name: "device", enabled: true, id: "forged", device: &linkeddevices.DevicePrincipal{DeviceID: "device", Scope: linkeddevices.ScopeWebV1, ApprovedBy: &pairing.Person{ID: "approver", Name: "Approver"}}, wantID: "approver", wantName: "Approver"},
		{name: "old device", enabled: true, id: "forged", device: &linkeddevices.DevicePrincipal{DeviceID: "device", Scope: linkeddevices.ScopeWebV1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &serveServer{}
			if tc.enabled {
				s.cfg.identityHeader = "Remote-User"
				s.cfg.identityNameHeader = "Remote-Name"
				s.cfg.identityEmailHeader = "Remote-Email"
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Remote-User", tc.id)
			req.Header.Set("Remote-Name", tc.display)
			req.Header.Set("Remote-Email", tc.email)
			if tc.device != nil {
				req = linkeddevices.WithDevicePrincipal(req, *tc.device)
			}
			s.auth(func(w http.ResponseWriter, r *http.Request) {
				got := identityFromContext(r.Context())
				if tc.wantID == "" {
					if got != nil {
						t.Fatalf("unexpected identity: %+v", got)
					}
					return
				}
				if got == nil || got.Author != (session.Author{ID: tc.wantID, Name: tc.wantName, Email: tc.wantEmail}) {
					t.Fatalf("identity = %+v", got)
				}
			})(httptest.NewRecorder(), req)
		})
	}
}

func TestIdentityAuthAndViewer(t *testing.T) {
	for _, bearer := range []bool{false, true} {
		s := &serveServer{cfg: serveServerConfig{identityHeader: "Remote-User", requireAuth: bearer, token: "secret"}}
		var etags []string
		for _, id := range []string{"alice", "bob", ""} {
			req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
			req.Header.Set("Remote-User", id)
			req.Header.Set("Authorization", "Bearer secret")
			w := httptest.NewRecorder()
			s.auth(func(w http.ResponseWriter, r *http.Request) {
				payload := map[string]any{}
				w.Header().Set("ETag", `W/"base"`)
				addViewerCapability(w, r, payload)
				writeJSON(w, 200, payload)
			})(w, req)
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			etags = append(etags, w.Header().Get("ETag"))
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if (body["viewer"] != nil) != (id != "") {
				t.Fatal(body)
			}
		}
		if etags[0] == etags[1] || etags[0] == etags[2] {
			t.Fatal(etags)
		}
	}
	if authorship.PairingApprover(context.Background()) != nil {
		t.Fatal("anonymous approver")
	}
}

func TestIdentityPasskeyAuthentication(t *testing.T) {
	s := &serveServer{browserAuth: &browserPasskeyHandler{}, cfg: serveServerConfig{identityHeader: "Remote-User"}}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Remote-User", "alice")
	handler := s.auth(func(w http.ResponseWriter, r *http.Request) {
		if identity := identityFromContext(r.Context()); identity == nil || identity.Author.ID != "alice" {
			t.Fatal("passkey identity missing")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	denied := httptest.NewRecorder()
	handler(denied, request)
	if denied.Code != http.StatusUnauthorized {
		t.Fatal("identity header bypassed passkeys")
	}
	accepted := httptest.NewRecorder()
	handler(accepted, withHubPrincipal(request, passkeyauth.Principal{SessionID: "session"}))
	if accepted.Code != http.StatusNoContent {
		t.Fatal(accepted.Code)
	}
}

func identityFromContext(ctx context.Context) *authorship.Identity {
	return authorship.FromContext(ctx)
}
