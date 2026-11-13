package oauth

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

func TestCallbackErrorCodesRemainSafe(t *testing.T) {
	for _, code := range []string{
		"invalid_request", "unauthorized_client", "access_denied", "unsupported_response_type",
		"invalid_scope", "server_error", "temporarily_unavailable", "invalid_client", "invalid_grant",
		"unsupported_grant_type", "invalid_target",
	} {
		t.Run(code, func(t *testing.T) {
			coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
			record := &flowRecord{flow: Flow{ID: "flow", State: FlowPending, ExpiresAt: time.Now().Add(time.Hour)}, callbackState: "state", callback: make(chan callbackResult, 1)}
			coordinator.flows["flow"] = record
			coordinator.byState["state"] = "flow"
			if id, ok := coordinator.CompleteCallback("state", "", "", code); !ok || id != "flow" {
				t.Fatal("callback rejected")
			}
			result := <-record.callback
			if got := safeError(fmt.Errorf("authorization: %w", result.err)); got != "authorization failed: "+code {
				t.Errorf("safe error = %q, want code %q", got, code)
			}
			if _, ok := coordinator.CompleteCallback("state", "", "", code); ok {
				t.Fatal("duplicate callback accepted")
			}
		})
	}
	for _, code := range []string{"arbitrary-secret", "access_denied: sensitive-description", "invalid_client\nsecret", "https://evil.example/?token=secret"} {
		if got := safeError(newCallbackError(code)); got != "authorization was denied" {
			t.Errorf("untrusted callback code leaked: %q", got)
		}
	}
}

func TestCallbackErrorFlowDoesNotExchangeCode(t *testing.T) {
	s := newIdentityTestServer(t, nil, false)
	coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
	flow, err := coordinator.Start(t.Context(), s.URL+"/resource", Options{HTTPClient: s.Client()}, "http://127.0.0.1:4321/callback", true)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(flow.AuthorizationURL)
	if _, ok := coordinator.CompleteCallback(u.Query().Get("state"), "unused-code", s.URL, "invalid_client"); !ok {
		t.Fatal("callback rejected")
	}
	completed, err := coordinator.Wait(t.Context(), flow.ID)
	if err != nil || completed.State != FlowFailed || completed.Error != "authorization failed: invalid_client" || s.tokens.Load() != 0 || s.registrations.Load() != 0 {
		t.Fatalf("callback error handling: flow=%+v err=%v tokens=%d DCR=%d", completed, err, s.tokens.Load(), s.registrations.Load())
	}
}
