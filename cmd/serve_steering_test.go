package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// No special Rush capability: wrappers and native adapters use ordinary Stream.
type rushContinuationProvider struct{ llm.Provider }

func TestRushHTTPSettlesThenStartsExactlyOneOrderedReplacement(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		name := "direct"
		if wrapped {
			name = "provider-without-special-rush-interface"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			entered := make(chan struct{})
			sourceStopped := make(chan struct{})
			replacementInput := make(chan []map[string]any, 1)
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []map[string]any `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
					w.(http.Flusher).Flush()
					close(entered)
					<-r.Context().Done()
					close(sourceStopped)
					return
				}
				select {
				case <-sourceStopped:
				default:
					t.Error("replacement overlapped source HTTP transport")
				}
				replacementInput <- body.Messages
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"steered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer model.Close()
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: ":memory:"})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			sess := &session.Session{ID: "rush-session", Provider: "compat", Model: "test", Mode: session.ModeChat}
			if err := store.Create(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			var provider llm.Provider = llm.NewOpenAICompatProvider(model.URL, "", "test", "compat")
			if wrapped {
				provider = rushContinuationProvider{provider}
			}
			registry := llm.NewToolRegistry()
			registry.Register(responseTimeoutDelayTool{})
			rt := &serveRuntime{provider: provider, providerKey: "compat", engine: llm.NewEngine(provider, registry), defaultModel: "test", store: store}
			rt.Touch()
			manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) { return rt, nil })
			defer manager.Close()
			manager.sessions[sess.ID] = rt
			srv := &serveServer{store: store, sessionMgr: manager, responseRuns: newServeResponseRunManager(), shutdownCh: make(chan struct{})}
			defer srv.responseRuns.Close()
			defer func() {
				if srv.responseLifecycleCancel != nil {
					srv.responseLifecycleCancel()
					srv.responseLifecycleWG.Wait()
				}
			}()
			run, err := srv.startResponseRun(rt, true, false, []llm.Message{llm.UserText("source")}, llm.Request{SessionID: sess.ID, Tools: []llm.ToolSpec{responseTimeoutDelayTool{}.Spec()}}, sess.ID, startResponseRunOptions{uiSession: true})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("provider not started")
			}
			for _, id := range []string{"z", "a"} {
				_, _, err := rt.InterruptMessage(context.Background(), llm.UserText("guidance "+id), "guidance "+id, id, nil, interruptDeliverySteer)
				if err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(map[string]any{"request_id": "rush-once", "expected_response_id": run.id, "expected_run_epoch": run.runEpoch})
			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sess.ID+"/steering/rush", strings.NewReader(string(body)))
			response := httptest.NewRecorder()
			srv.handleSteeringRush(response, req, sess.ID, "")
			if response.Code != 202 {
				t.Fatalf("admission %d: %s", response.Code, response.Body.String())
			}
			var op *session.RushOperation
			waitForServeCondition(t, 3*time.Second, func() bool {
				op, _ = store.GetRush(context.Background(), sess.ID, "rush-once")
				return op != nil && !op.Status.Active()
			}, "rush to start")
			if op.Status != session.RushStarted {
				t.Fatalf("rush = %+v", op)
			}
			select {
			case messages := <-replacementInput:
				raw, _ := json.Marshal(messages)
				text := string(raw)
				if !strings.Contains(text, "guidance z") || !strings.Contains(text, "guidance a") || strings.Index(text, "guidance z") > strings.Index(text, "guidance a") {
					t.Fatalf("FIFO lost: %s", text)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("replacement not started")
			}
			retry := httptest.NewRecorder()
			srv.handleSteeringRush(retry, httptest.NewRequest(http.MethodPost, req.URL.String(), strings.NewReader(string(body))), sess.ID, "")
			if retry.Code != 200 {
				t.Fatalf("retry %d: %s", retry.Code, retry.Body.String())
			}
			if calls.Load() != 2 {
				t.Fatalf("provider calls = %d", calls.Load())
			}
			rows, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]int{}
			for _, row := range rows {
				ids[row.ClientMessageID]++
			}
			if ids["z"] != 1 || ids["a"] != 1 {
				t.Fatalf("durable identities: %+v", ids)
			}
		})
	}
}

func TestSteeringWireProjectionPreservesUserContentAndSequence(t *testing.T) {
	event := responseRunEvent{Sequence: 42, Event: "response.interjection", Data: []byte(`{"type":"response.interjection","interjection_id":"id","text":"interject literally","sequence_number":42}`)}
	for _, canonical := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		writer := &steeringWireWriter{ResponseWriter: recorder, canonical: canonical}
		if err := writeStoredResponseEvent(writer, event); err != nil {
			t.Fatal(err)
		}
		want := "response.interjection"
		if canonical {
			want = "response.steering"
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "event: "+want) || !strings.Contains(body, "id: 42") || !strings.Contains(body, "interject literally") {
			t.Fatalf("projection: %s", body)
		}
	}
}
