package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSynchronousHumanAdmissionSupersedesHeldCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		busy         bool
		wantStatus   int
		wantEligible bool
	}{
		{"accepted human", `{"input":"next human turn","stream":false}`, false, http.StatusOK, false},
		{"accepted provider failure", `{"input":"next human turn","stream":false}`, false, http.StatusBadRequest, false},
		{"busy human", `{"input":"next human turn","stream":false}`, true, http.StatusConflict, true},
		{"rejected human", `{"input":"next human turn","stream":false,"branch":true}`, false, http.StatusBadRequest, true},
		{"rejected runtime setup", `{"input":"next human turn","stream":false}`, false, http.StatusBadRequest, true},
		{"autonomous input", `{"input":[{"type":"message","role":"developer","content":"background bookkeeping"}],"stream":false}`, false, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			provider := mockprovider.NewMockProvider("test")
			if tc.name == "accepted provider failure" {
				provider.AddError(errors.New("synthetic provider failure"))
			} else {
				provider.AddTextResponse("done")
			}
			runtime := &serveRuntime{provider: provider, engine: engine.NewEngine(provider, nil), defaultModel: "test", store: store}
			runtime.Touch()
			mgr := newServeSessionManager(time.Minute, 100, func(context.Context) (*serveRuntime, error) { return runtime, nil })
			defer mgr.Close()
			s := &serveServer{sessionMgr: mgr, store: store, responseRuns: newServeResponseRunManager()}
			defer s.responseRuns.Close()
			sub, err := store.UpsertPushSubscription(ctx, &session.PushSubscription{Endpoint: "https://example.invalid/never-sent", KeyP256DH: "key", KeyAuth: "auth"})
			if err != nil {
				t.Fatal(err)
			}
			seedCompletionCandidate(t, s, store, "old", "human-session", sub.ID)
			claimed, err := store.ClaimCompletionNotifications(ctx, "test", time.Now().Add(4*time.Second), time.Minute, 1)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("held candidate=%+v err=%v", claimed, err)
			}
			candidate := claimed[0]
			if tc.busy {
				runtime.mu.Lock()
				defer runtime.mu.Unlock()
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			if tc.name == "rejected runtime setup" {
				r = r.WithContext(withServeRuntimeSetup(r.Context(), func(*core.Request) error {
					return errors.New("synthetic runtime setup rejection")
				}))
			}
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(requestSessionIDHeader, "human-session")
			w := httptest.NewRecorder()
			s.handleResponses(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
			eligible, err := store.RecheckCompletionNotification(ctx, candidate.ID, candidate.ClaimOwner, candidate.ClaimToken, time.Now())
			if err != nil && !(errors.Is(err, session.ErrNotificationClaimLost) && !tc.wantEligible) {
				t.Fatalf("recheck old candidate: %v", err)
			}
			if eligible != tc.wantEligible {
				t.Fatalf("old candidate eligible=%v want=%v", eligible, tc.wantEligible)
			}
			if tc.wantEligible && tc.wantStatus != http.StatusOK && provider.CurrentTurn() != 0 {
				t.Fatal("rejected request invoked provider")
			}
		})
	}
}
