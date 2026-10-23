package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

type notificationNativeIdentityVector struct {
	PersistentIdentifier string `json:"persistent_identifier"`
	WindowID             string `json:"window_id"`
}

func notificationNativeIdentityVectors(t *testing.T) []notificationNativeIdentityVector {
	t.Helper()
	body, err := os.ReadFile("testdata/notification_native_identity.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Version         int                                `json:"version"`
		Algorithm       string                             `json:"algorithm"`
		AcceptedPattern string                             `json:"accepted_pattern"`
		Vectors         []notificationNativeIdentityVector `json:"vectors"`
	}
	if err := json.Unmarshal(body, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Version != 1 || contract.Algorithm != "native_ + unpadded base64url(SHA256(UTF8(persistent_identifier)))" || contract.AcceptedPattern != "^[A-Za-z0-9_-]{8,100}$" || len(contract.Vectors) != 3 {
		t.Fatalf("unexpected synthetic native identity contract: %+v", contract)
	}
	return contract.Vectors
}

// These synthetic vectors pin the cross-client algorithm contract. Computing
// its Go equivalent here does not execute a native client implementation.
func TestNotificationNativeIdentityHTTPBoundary(t *testing.T) {
	for i, vector := range notificationNativeIdentityVectors(t) {
		t.Run(fmt.Sprintf("vector_%d", i), func(t *testing.T) {
			digest := sha256.Sum256([]byte(vector.PersistentIdentifier))
			encoded := "native_" + base64.RawURLEncoding.EncodeToString(digest[:])
			if encoded != vector.WindowID || len(encoded) != 50 || !regexp.MustCompile(`^[A-Za-z0-9_-]{8,100}$`).MatchString(encoded) {
				t.Fatalf("native identity contract mismatch: computed=%q vector=%q", encoded, vector.WindowID)
			}
			provider := mockprovider.NewMockProvider("debug").AddTextResponse("Synthetic native response")
			s, store, _, origin := newNotificationNativeFixture(t, provider, vector.WindowID)
			if w := postNotificationNativePresence(t, s, origin); w.Code != http.StatusOK {
				t.Fatalf("presence=%d %s", w.Code, w.Body.String())
			}
			r := notificationNativeRequest(origin, "/v1/responses", `{"input":"Synthetic native human turn","stream":true,"client_message_id":"native-human-1"}`)
			w := httptest.NewRecorder()
			s.handleResponses(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("human response=%d %s", w.Code, w.Body.String())
			}
			run := waitNotificationNativeRun(t, s, w.Header().Get("x-response-id"))
			payload := assertNotificationNativeCandidate(t, s, store, origin, run)
			t.Run("managed_producer_to_worker", func(t *testing.T) {
				node, err := exec.LookPath("node")
				if err != nil {
					t.Skip("node required for actual worker contract")
				}
				fixture := filepath.Join(t.TempDir(), "managed-completion.json")
				if err := os.WriteFile(fixture, payload, 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				// Mount the real worker at this synthetic producer's public scope.
				workerContract := strings.Replace(completionWorkerContract, "https://example.test/ui/", "https://example.test"+origin.Source+"/", 1)
				if output, err := exec.CommandContext(ctx, node, "-e", workerContract, fixture, filepath.Join("..", "..", "serveui", "static", "sw.js")).CombinedOutput(); err != nil {
					t.Fatalf("actual managed producer → worker: %v\n%s", err, output)
				}
			})
			if provider.CurrentTurn() != 1 {
				t.Fatalf("inference count=%d, want 1", provider.CurrentTurn())
			}
		})
	}
}

func TestNotificationNativeColonIdentityFailsClosed(t *testing.T) {
	provider := mockprovider.NewMockProvider("debug").AddTextResponse("Still a human response, without OS authority")
	s, store, _, origin := newNotificationNativeFixture(t, provider, "native:synthetic-scene")
	if w := postNotificationNativePresence(t, s, origin); w.Code != http.StatusBadRequest {
		t.Fatalf("colon-bearing presence=%d %s, want 400", w.Code, w.Body.String())
	}
	r := notificationNativeRequest(origin, "/v1/responses", `{"input":"Synthetic invalid-owner human turn","stream":true,"client_message_id":"native-invalid-1"}`)
	w := httptest.NewRecorder()
	s.handleResponses(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("human response=%d %s", w.Code, w.Body.String())
	}
	waitNotificationNativeRun(t, s, w.Header().Get("x-response-id"))
	rows, err := store.ClaimCompletionNotifications(context.Background(), "native-contract", time.Now().Add(4*time.Second), time.Minute, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid identity acquired automatic notification authority: candidates=%+v err=%v", rows, err)
	}
}

func newNotificationNativeFixture(t *testing.T, provider *mockprovider.MockProvider, windowID string) (*serveServer, *session.SQLiteStore, *serveRuntime, session.NotificationOrigin) {
	t.Helper()
	t.Cleanup(SwapProcessSessionInputs(NewSessionInputCoordinator()))
	s, store, rt := newAgentWakeFixture(t, provider)
	// First-party HTTP admission may replace the warmed runtime. The wake
	// fixture's singleton factory cannot be used while that runtime is pinned.
	s.runtimeFactory = func(context.Context, serveRuntimeRequest) (*serveRuntime, error) {
		return &serveRuntime{provider: provider, providerKey: "debug", engine: engine.NewEngine(provider, nil), store: store, defaultModel: "fast", platform: "web"}, nil
	}
	s.cfgRef.DefaultProvider = "debug"
	s.shutdownCh = make(chan struct{})
	// Exercise explicit HTTP admission, not a competing startup reconciler.
	s.responseLifecycleOnce.Do(func() {})
	t.Cleanup(func() { close(s.shutdownCh) })
	s.cfgRef.Serve.WebPush.VAPIDPublicKey = "test-key"
	sub, err := store.UpsertPushSubscription(context.Background(), &session.PushSubscription{
		Endpoint: "https://example.invalid/native-contract", KeyP256DH: "key", KeyAuth: "auth", VAPIDKeyID: appnotify.WebPushKeyID("test-key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, store, rt, session.NotificationOrigin{SubscriptionID: sub.ID, WindowID: windowID, DocumentID: "document_native_contract", Source: "/node/Synthetic"}
}

func postNotificationNativePresence(t *testing.T, s *serveServer, origin session.NotificationOrigin) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(session.NotificationPresence{
		Version: 2, WindowID: origin.WindowID, DocumentID: origin.DocumentID, SubscriptionID: origin.SubscriptionID,
		Source: origin.Source, Sequence: 1, Active: false, ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/push/presence", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleNotificationPresence(w, r)
	return w
}

func notificationNativeRequest(origin session.NotificationOrigin, path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(requestSessionIDHeader, "wake-parent")
	r.Header.Set("X-Term-LLM-UI-Version", "test")
	r.Header.Set(requestNotificationVersionHeader, "2")
	r.Header.Set(requestPushSubscriptionHeader, origin.SubscriptionID)
	r.Header.Set(requestNotificationWindowHeader, origin.WindowID)
	r.Header.Set(requestNotificationDocumentHeader, origin.DocumentID)
	r.Header.Set(requestNotificationSourceHeader, origin.Source)
	return r
}

func waitNotificationNativeRun(t *testing.T, s *serveServer, responseID string) *responseRun {
	t.Helper()
	run, ok := s.responseRuns.get(responseID)
	if !ok {
		t.Fatalf("accepted response run %q not found", responseID)
	}
	subscription := run.subscribe(0)
	if subscription.ch != nil {
		defer run.unsubscribe(subscription.ch)
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
	waiting:
		for {
			select {
			case _, open := <-subscription.ch:
				if !open {
					break waiting
				}
			case <-timer.C:
				t.Fatal("accepted response did not finish")
			}
		}
	}
	if snapshot := run.snapshot(); snapshot["status"] != "completed" {
		t.Fatalf("accepted response did not complete: %+v", snapshot)
	}
	return run
}

func assertNotificationNativeCandidate(t *testing.T, s *serveServer, store *session.SQLiteStore, origin session.NotificationOrigin, run *responseRun) []byte {
	t.Helper()
	rows, err := store.ClaimCompletionNotifications(context.Background(), "native-contract", time.Now().Add(4*time.Second), time.Minute, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("real managed completion candidates=%+v err=%v", rows, err)
	}
	candidate := rows[0]
	epoch := run.snapshot()["run_epoch"].(int64)
	if epoch <= 0 || candidate.RunEpoch != epoch || candidate.ResponseID != run.id || candidate.SessionID != "wake-parent" || candidate.SubscriptionID != origin.SubscriptionID || candidate.WindowID != origin.WindowID || candidate.DocumentID != origin.DocumentID || candidate.Source != origin.Source || candidate.Outcome != session.ResponseRunCompleted {
		t.Fatalf("managed completion lost admitted owner/epoch: candidate=%+v origin=%+v epoch=%d", candidate, origin, epoch)
	}
	body, err := s.completionNotificationPayload(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var envelope completionPushPayload
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	// The positive epoch comes from actual managed admission/finalization, not a
	// manually seeded lifecycle row. It must survive the real JSON producer.
	if envelope.Version != 2 || envelope.Kind != "completion" || envelope.RunEpoch <= 0 || envelope.RunEpoch != epoch || envelope.WindowID != origin.WindowID || envelope.Source != origin.Source || envelope.ResponseID != run.id || envelope.TurnID == "" || envelope.TurnID != candidate.TurnID || envelope.EventID != candidate.EventID || envelope.StoreInstanceID == "" || envelope.StoreInstanceID != candidate.StoreInstanceID || envelope.SessionID != candidate.SessionID || envelope.AttentionSeq <= 0 || envelope.AttentionSeq != candidate.AttentionSeq || envelope.Outcome != "completed" || envelope.URL != origin.Source+"/chat/wake-parent" {
		t.Fatalf("real completion producer lost owner/read/epoch contract: %s", body)
	}
	return body
}
