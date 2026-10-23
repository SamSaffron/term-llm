package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

// Exercise the first-party streaming route, not just the model-swap helpers:
// successful turn -> failed turn with no assistant -> model change, without undo.
func TestModelSwapAfterFailedResponse(t *testing.T) {
	for _, cold := range []bool{false, true} {
		for _, fallback := range []bool{false, true} {
			name := "warm/naive"
			if cold {
				name = "cold/naive"
			}
			if fallback {
				name = strings.Replace(name, "naive", "handover", 1)
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				source := mockprovider.NewMockProvider("old").AddTextResponse("previous answer").AddError(errors.New("Responses API error (cyber_policy): request rejected"))
				target := mockprovider.NewMockProvider("new")
				if fallback {
					target.AddError(errors.New("400 invalid_request: incompatible reasoning"))
				}
				target.AddTextResponse("recovered on target")
				helper := mockprovider.NewMockProvider("old").AddTextResponse("handover summary")
				makeRuntime := func(provider *mockprovider.MockProvider, key, model string) *serveRuntime {
					rt := &serveRuntime{provider: provider, providerKey: key, defaultModel: model, engine: engine.NewEngine(provider, nil), store: store}
					rt.Touch()
					return rt
				}
				previous := makeRuntime(source, "old", "old-model")
				manager := newServeSessionManager(time.Minute, 100, nil)
				t.Cleanup(manager.Close)
				srv := &serveServer{cfgRef: &config.Config{DefaultProvider: "old"}, sessionMgr: manager, store: store, responseRuns: newServeResponseRunManager()}
				t.Cleanup(func() { srv.responseRuns.Close() })
				srv.runtimeFactory = func(_ context.Context, req serveRuntimeRequest) (*serveRuntime, error) {
					if req.Provider == "new" {
						return makeRuntime(target, "new", "new-model"), nil
					}
					if req.SessionID == "" {
						return makeRuntime(helper, "old", "old-model"), nil
					}
					return previous, nil
				}
				const sessionID = "failed-swap"
				send := func(body map[string]any) map[string]any {
					t.Helper()
					body["stream"] = true
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Term-LLM-UI-Version", "test")
					req.Header.Set("session_id", sessionID)
					rr := httptest.NewRecorder()
					srv.handleResponses(rr, req)
					if rr.Code != http.StatusOK {
						t.Fatalf("request failed: %d %s", rr.Code, rr.Body.String())
					}
					run, ok := srv.responseRuns.get(rr.Header().Get("x-response-id"))
					if !ok {
						t.Fatalf("no admitted response: %s", rr.Body.String())
					}
					select {
					case <-run.settled:
					case <-time.After(5 * time.Second):
						t.Fatal("response did not settle")
					}
					scanner := bufio.NewScanner(strings.NewReader(rr.Body.String()))
					for {
						event, data, ok := readSSEEvent(t, scanner)
						if !ok || data == "[DONE]" {
							break
						}
						if event == "response.completed" || event == "response.failed" {
							var terminal map[string]any
							if err := json.Unmarshal([]byte(data), &terminal); err != nil {
								t.Fatal(err)
							}
							terminal["event"] = event
							return terminal
						}
					}
					t.Fatalf("no terminal event: %s", rr.Body.String())
					return nil
				}
				first := send(map[string]any{"input": "first question", "client_message_id": "first", "provider": "old", "model": "old-model"})
				if first["event"] != "response.completed" {
					t.Fatalf("initial turn failed: %#v", first)
				}
				anchor := srv.latestDurableResponseIDForSession(ctx, sessionID)
				failed := send(map[string]any{"input": "failed question", "client_message_id": "failed", "previous_response_id": anchor})
				if failed["event"] != "response.failed" {
					t.Fatalf("expected failed turn: %#v", failed)
				}
				stored, err := store.GetMessages(ctx, sessionID, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(stored) < 2 || stored[len(stored)-2].Role != core.RoleUser {
					t.Fatalf("failed turn has unexpected tail: %#v", stored)
				}
				if _, ok := llm.ParseRunErrorMarker(stored[len(stored)-1].ToLLMMessage()); !ok {
					t.Fatal("missing durable error event")
				}
				if cold {
					manager.mu.Lock()
					delete(manager.sessions, sessionID)
					manager.mu.Unlock()
					previous.Close()
					previous = makeRuntime(mockprovider.NewMockProvider("old"), "old", "old-model")
				}
				// The terminal event carries the durable continuation cursor even
				// though its response_id still identifies the live transport run.
				anchor = failed["response"].(map[string]any)["id"].(string)
				if anchor != durableResponseIDForMessageID(stored[len(stored)-2].ID) {
					t.Fatalf("failure continuation = %q, want failed user row", anchor)
				}
				if cold {
					anchor = srv.latestDurableResponseIDForSession(ctx, sessionID)
				}
				recovered := send(map[string]any{"input": "continue on another model", "client_message_id": "recovery", "previous_response_id": anchor, "provider": "new", "model": "new-model", "model_swap": map[string]any{"mode": "auto", "fallback": "handover"}})
				if recovered["event"] != "response.completed" {
					t.Fatalf("model change after error failed: %#v", recovered)
				}
				sess, err := store.Get(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				if sess.ProviderKey != "new" || sess.Model != "new-model" {
					t.Fatalf("model change not committed: %s/%s", sess.ProviderKey, sess.Model)
				}
				if !fallback {
					messages, err := store.GetMessages(ctx, sessionID, 0, 0)
					if err != nil {
						t.Fatal(err)
					}
					foundError := false
					for _, message := range messages {
						if _, ok := llm.ParseRunErrorMarker(message.ToLLMMessage()); ok {
							foundError = true
						}
					}
					if !foundError {
						t.Fatal("model swap erased the earlier failed-turn error event")
					}
				}
				requests := target.RecordedRequests()
				wantRequests := 1
				if fallback {
					wantRequests = 2
					helperRequests := helper.RecordedRequests()
					if len(requests) != wantRequests || len(helperRequests) != 1 || !requestContainsText(helperRequests[0], "failed question") || !requestContainsText(requests[len(requests)-1], "handover summary") {
						t.Fatal("handover did not carry the failed turn into the summary retry")
					}
				}
				if len(requests) != wantRequests || !requestContainsText(requests[0], "failed question") || !requestContainsText(requests[0], "continue on another model") {
					t.Fatal("target did not receive failed user turn and recovery prompt")
				}
				for _, req := range requests {
					for _, msg := range req.Messages {
						if msg.Role == core.RoleEvent {
							t.Fatal("transcript event sent to target")
						}
					}
				}
			})
		}
	}
}
