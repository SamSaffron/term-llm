package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestResponsesEffortOnlyChangeRetargetsOpenCompatWithoutSwap(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/fail=%v", streaming, fail), func(t *testing.T) {
				ctx := context.Background()
				srv, store := newServeProjectTestServer(t)
				srv.projectsEnabled = false
				srv.cfgRef = &config.Config{DefaultProvider: "flashnext"}
				captured := make(chan string, 4)
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Effort string `json:"reasoning_effort"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					captured <- body.Effort
					if fail {
						http.Error(w, `{"error":{"message":"injected inference failure"}}`, http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}))
				t.Cleanup(backend.Close)
				provider := opencompat.NewOpenAICompatProvider(backend.URL, "", "flashnext", "flashnext")
				provider.SetModelConfigs([]config.ProviderModelConfig{{ID: "flashnext", ReasoningEfforts: []string{"none", "low", "medium", "xhigh"}, DefaultReasoningEffort: "none"}})
				rt := &serveRuntime{provider: provider, providerKey: "flashnext", defaultModel: "flashnext", engine: engine.NewEngine(provider, nil), store: store}
				rt.Touch()
				sess := &session.Session{ID: session.NewID(), Provider: "flashnext", ProviderKey: "flashnext", Model: "flashnext", ReasoningEffort: "medium", Mode: session.ModeChat}
				if err := store.Create(ctx, sess); err != nil {
					t.Fatal(err)
				}
				for _, msg := range []core.Message{core.UserText("old question"), core.AssistantText("old answer")} {
					if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
						t.Fatal(err)
					}
				}
				// Model an already-prepared web runtime; input refresh is unrelated
				// to changing reasoning effort and must not obscure swap allocation.
				t.Cleanup(SwapProcessSessionInputs(NewSessionInputCoordinator()))
				ticket, err := ProcessSessionInputs().Acquire(ctx, store, sess.ID, InputBinding("", ""))
				if err != nil {
					t.Fatal(err)
				}
				rt.inputs.Store(ticket.Finish(SessionInputSelection{}))
				manager := newServeSessionManager(time.Minute, 10, nil)
				t.Cleanup(manager.Close)
				srv.sessionMgr = manager
				putTestSession(manager, sess.ID, rt)
				srv.runtimeFactory = func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
					t.Errorf("effort-only change attempted runtime construction: %+v", request)
					return nil, errors.New("unexpected candidate")
				}
				anchor := srv.latestDurableResponseIDForSession(ctx, sess.ID)
				body := fmt.Sprintf(`{"input":"new question","client_message_id":"effort-change","previous_response_id":%q,"provider":"flashnext","model":"flashnext","reasoning_effort":"xhigh","model_swap":{"mode":"auto","fallback":"handover"},"stream":%v}`, anchor, streaming)
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Term-LLM-UI-Version", "test")
				req.Header.Set("session_id", sess.ID)
				rr := httptest.NewRecorder()
				srv.handleResponses(rr, req)
				if streaming {
					run, ok := srv.responseRuns.get(rr.Header().Get("x-response-id"))
					if !ok {
						t.Fatalf("no admitted response: %d %s", rr.Code, rr.Body.String())
					}
					t.Cleanup(srv.responseRuns.Close)
					select {
					case <-run.settled:
					case <-time.After(5 * time.Second):
						t.Fatal("response did not settle")
					}
					wantStatus := "completed"
					if fail {
						wantStatus = "failed"
					}
					if got := run.snapshot()["status"]; got != wantStatus {
						t.Fatalf("response status = %v, want %s", got, wantStatus)
					}
				} else if (!fail && rr.Code != http.StatusOK) || (fail && rr.Code != http.StatusBadRequest) {
					t.Fatalf("unexpected response: %d %s", rr.Code, rr.Body.String())
				}
				select {
				case effort := <-captured:
					if effort != "xhigh" {
						t.Fatalf("wire reasoning_effort = %q, want xhigh", effort)
					}
				default:
					t.Fatal("provider did not receive a request")
				}
				stored, err := store.Get(ctx, sess.ID)
				if err != nil || stored.ReasoningEffort != "xhigh" {
					t.Fatalf("accepted effort not persisted despite inference outcome: session=%+v err=%v", stored, err)
				}
				if current, ok := manager.Get(sess.ID); !ok || current != rt {
					t.Fatal("effort-only change replaced the runtime")
				}
				messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				if !sessionMessagesContainText(messages, "new question") {
					t.Fatal("inference failure removed the admitted input")
				}
				for _, msg := range messages {
					if _, ok := core.ParseModelSwapMarker(msg.ToLLMMessage()); ok {
						t.Fatal("effort-only change used model-swap machinery")
					}
				}
			})
		}
	}
}

func TestEffortOnlyChangeDefaultAndAdmission(t *testing.T) {
	ctx := context.Background()
	_, store := newServeProjectTestServer(t)
	sess := &session.Session{ID: session.NewID(), Provider: "flashnext", ProviderKey: "flashnext", Model: "flashnext", ReasoningEffort: "medium", Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	provider := mockprovider.NewMockProvider("flashnext").AddError(errors.New("inference failure"))
	rt := &serveRuntime{provider: provider, providerKey: "flashnext", defaultModel: "flashnext", engine: engine.NewEngine(provider, nil), store: store}
	changeCtx := withResponseEffortChange(ctx, sess.ID, "")
	rt.mu.Lock()
	_, err := rt.Run(changeCtx, true, false, []core.Message{core.UserText("busy")}, core.Request{SessionID: sess.ID})
	rt.mu.Unlock()
	if !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("busy Run error = %v", err)
	}
	stored, err := store.Get(ctx, sess.ID)
	if err != nil || stored.ReasoningEffort != "medium" {
		t.Fatalf("busy run changed effort: session=%+v err=%v", stored, err)
	}
	if err := rt.acceptResponseEffortChange(changeCtx, "another-session"); err != nil {
		t.Fatal(err)
	}
	_, err = rt.Run(changeCtx, true, false, []core.Message{core.UserText("use provider default")}, core.Request{SessionID: sess.ID})
	if err == nil {
		t.Fatal("expected inference failure")
	}
	stored, err = store.Get(ctx, sess.ID)
	if err != nil || stored.ReasoningEffort != "" {
		t.Fatalf("provider default not accepted: session=%+v err=%v", stored, err)
	}
}

func TestIsEffortOnlySwapRequiresSameProviderAndModel(t *testing.T) {
	for _, tc := range []struct {
		provider, model, previousModel string
		want                           bool
	}{
		{"flashnext", "flashnext", "flashnext", true},
		{"flashnext", "", "flashnext", true},
		{"flashnext", "new", "", false},
		{"flashnext", "new", "flashnext", false},
		{"other", "flashnext", "flashnext", false},
	} {
		plan := responseModelSwapPlan{enabled: true, previousProvider: "flashnext", previousModel: tc.previousModel, previousEffort: "medium", requestedProvider: tc.provider, requestedModel: tc.model, requestedEffort: "xhigh"}
		if got := isEffortOnlySwap(plan); got != tc.want {
			t.Errorf("isEffortOnlySwap(%+v) = %v, want %v", plan, got, tc.want)
		}
	}
}
