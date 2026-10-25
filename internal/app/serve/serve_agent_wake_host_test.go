package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestAgentWakeHeadlessLeavesEventForWebHost(t *testing.T) {
	for _, origin := range []session.SessionOrigin{session.OriginWeb, session.OriginTUI} {
		t.Run(string(origin), func(t *testing.T) {
			ctx := context.Background()
			webProvider := mockprovider.NewMockProvider("debug").AddToolCall("wake-question", tools.AskUserToolName, map[string]any{
				"questions": []map[string]any{{"header": "Theme", "question": "Pick a theme", "options": []map[string]any{
					{"label": "Dark", "description": "Dark mode"}, {"label": "Light", "description": "Light mode"},
				}}},
			}).AddTextResponse("All set.")
			web, db, webRT := newAgentWakeFixture(t, webProvider)
			webRT.engine.RegisterTool(tools.NewAskUserTool())
			meta, err := db.Get(ctx, "wake-parent")
			if err != nil {
				t.Fatal(err)
			}
			meta.Origin = origin
			if err := db.Update(ctx, meta); err != nil {
				t.Fatal(err)
			}
			store := db

			// A separate host shares durable storage, but not runtimes or response runs.
			headlessProvider := mockprovider.NewMockProvider("debug").AddTextResponse("stolen wake")
			headlessRT := &serveRuntime{provider: headlessProvider, engine: engine.NewEngine(headlessProvider, nil), store: db, defaultModel: "fast", platform: "web", sessionMeta: meta}
			var created atomic.Int32
			mgr := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
				created.Add(1)
				return headlessRT, nil
			})
			// Even a TUI parent previously loaded as a web runtime must not grant
			// automatic UI ownership to an API-only/jobs-only host.
			if origin == session.OriginTUI {
				putTestSession(mgr, meta.ID, headlessRT)
			}
			headless := &serveServer{store: store, sessionMgr: mgr, responseRuns: newServeResponseRunManager(), cfgRef: web.cfgRef}
			t.Cleanup(func() { headless.agentWakeWG.Wait(); headless.responseRuns.Close(); mgr.Close() })
			putWakeRun(t, db, "completed")
			before, err := db.GetAgentRun(ctx, "wake-child")
			if err != nil {
				t.Fatal(err)
			}

			if headless.ownsAgentWakeParent(meta) {
				t.Error("headless host claimed automatic parent ownership")
			}
			if headless.agentWakeStillPending(ctx, meta.ID, []session.AgentRun{before}, false) {
				t.Error("headless admission revalidation accepted event")
			}
			headless.reconcileAgentWakes(ctx)
			headless.agentWakeWG.Wait()
			headless.wakeAgentParent(meta.ID)
			headless.agentWakeWG.Wait()
			if !headless.deliverAgentWake(meta.ID) {
				t.Error("ineligible host requested retry")
			}
			after, err := db.GetAgentRun(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("headless host mutated pending event: before=%+v after=%+v", before, after)
			}
			if created.Load() != 0 || headlessProvider.CurrentTurn() != 0 || headless.responseRuns.activeRunID(meta.ID) != "" {
				t.Fatalf("headless side effects: runtimes=%d provider turns=%d", created.Load(), headlessProvider.CurrentTurn())
			}

			// Bound failed ask_user cleanup without enabling the fixture's
			// periodic lifecycle scanner; reconciliation is driven explicitly.
			web.shutdownCh = make(chan struct{})
			web.wakeAgentParent(meta.ID)
			waitForServeCondition(t, 3*time.Second, func() bool { return len(webRT.pendingAskUserPrompts()) == 1 }, "web wake question")
			rr := httptest.NewRecorder()
			web.handleSessionByID(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/"+meta.ID+"/state", nil))
			var state struct {
				ActiveRun        bool                `json:"active_run"`
				ActiveResponseID string              `json:"active_response_id"`
				PendingAskUser   *serveAskUserPrompt `json:"pending_ask_user"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
			if rr.Code != http.StatusOK || !state.ActiveRun || state.ActiveResponseID == "" || state.PendingAskUser == nil || state.PendingAskUser.CallID != "wake-question" || state.PendingAskUser.Questions[0].Question != "Pick a theme" {
				t.Fatalf("web state missing active question: %d %s", rr.Code, rr.Body.String())
			}
			rr = httptest.NewRecorder()
			answerReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+meta.ID+"/ask_user", strings.NewReader(`{"call_id":"wake-question","answers":[{"question_index":0,"header":"Theme","selected":"Dark"}]}`))
			answerReq.Header.Set("Content-Type", "application/json")
			web.handleSessionByID(rr, answerReq)
			if rr.Code != http.StatusOK {
				t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
			}
			web.agentWakeWG.Wait()
			run, ok := web.responseRuns.get(state.ActiveResponseID)
			if !ok {
				t.Fatal("missing web response")
			}
			run.mu.Lock()
			status := run.status
			run.mu.Unlock()
			after, err = db.GetAgentRun(ctx, before.ID)
			if err != nil || after.NotifiedAt.IsZero() || status != "completed" || webProvider.CurrentTurn() != 2 {
				t.Fatalf("web completion: event=%+v err=%v status=%s turns=%d", after, err, status, webProvider.CurrentTurn())
			}
			acknowledgedAt := after.NotifiedAt
			web.reconcileAgentWakes(ctx)
			web.agentWakeWG.Wait()
			after, err = db.GetAgentRun(ctx, before.ID)
			if err != nil || !after.NotifiedAt.Equal(acknowledgedAt) || webProvider.CurrentTurn() != 2 {
				t.Fatal("event delivered more than once")
			}
		})
	}
}
