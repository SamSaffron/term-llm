package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type childRunFixture struct {
	srv    *serveServer
	store  session.Store
	parent *session.Session
	env    *cmdRunEnvironment
	spawn  *tools.SpawnAgentTool
	ctx    context.Context
}

// newChildRunFixture builds the same runtime the web server builds, optionally
// with the child-run registry installed. Running both ways in one test file is
// what makes the containment claim checkable rather than assumed.
func newChildRunFixture(t *testing.T, withObserver bool) *childRunFixture {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	parent := &session.Session{ID: "child-run-parent", Provider: "debug", Model: "fast", Mode: session.ModeChat}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	agentDir := filepath.Join(root, "drill-child")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// More than one turn so the child engine opens a steering-accepting window;
	// a single-turn run is non-consuming from the start by design.
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: drill-child\nmodel: debug:fast\nskills: none\nmax_turns: 4\ntools:\n  enabled: [read_file]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		DefaultProvider: "debug",
		Providers:       map[string]config.ProviderConfig{"debug": {Model: "fast", FastModel: "fast"}},
		Agents:          config.AgentsConfig{SearchPaths: []string{root}},
	}
	srv := &serveServer{store: store}
	opts := serveAgentRuntimeOptions{cfg: cfg, cmd: &cobra.Command{}, store: store}
	if withObserver {
		opts.childRuns = srv.ensureChildRuns()
	}
	defaults := serveRuntimeRunnerDefaults(opts, serveRuntimeRequest{SessionID: parent.ID}, tools.ModePrompt)
	cfg.Sessions = config.SessionsConfig{Enabled: true, Path: dbPath}
	defaults.Tools = "spawn_agent"
	defaults.ToolsSet = true

	runner := newCmdRunner(cfg, defaults).(*cmdRunner)
	env, err := runner.prepare(ctx, runpkg.Request{
		Platform: runpkg.PlatformWeb, SessionID: parent.ID, Provider: "debug", Model: "fast",
		Cwd: t.TempDir(), DeferSession: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Close() })
	spawn := env.runtime.toolMgr.GetSpawnAgentTool()
	if spawn == nil {
		t.Fatal("spawn tool missing")
	}
	srv.sessionMgr = &serveSessionManager{sessions: map[string]*serveRuntime{parent.ID: env.runtime}}
	return &childRunFixture{
		srv: srv, store: store, parent: parent, env: env, spawn: spawn,
		ctx: llm.ContextWithCallID(llm.ContextWithSessionID(ctx, parent.ID), "spawn-drill"),
	}
}

func (f *childRunFixture) spawnChild(t *testing.T) tools.SpawnAgentResult {
	t.Helper()
	output, err := f.spawn.Execute(f.ctx, json.RawMessage(`{"agent_name":"drill-child","prompt":"Say hello","model":"debug:fast"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result tools.SpawnAgentResult
	if err := json.Unmarshal([]byte(output.Content), &result); err != nil {
		t.Fatalf("spawn result = %s, err = %v", output.Content, err)
	}
	return result
}

// Production constructs the runtime factory before it assigns the HTTP server.
// Supplying a registry directly in the fixture hides an eager nil capture here.
func TestServeRuntimeFactoryRegistersChildBeforeResult(t *testing.T) {
	f := newChildRunFixture(t, false)
	oldTools := serveTools
	serveTools = "spawn_agent"
	t.Cleanup(func() { serveTools = oldTools })
	command := &cobra.Command{}
	command.Flags().String("tools", "spawn_agent", "")
	if err := command.Flags().Set("tools", "spawn_agent"); err != nil {
		t.Fatal(err)
	}

	var srv *serveServer
	factory := newServeAgentRuntimeFactory(serveAgentRuntimeOptions{
		cfg: f.env.cfg, cmd: command, store: f.store,
		approval: resolvedApprovalMode{Mode: tools.ModePrompt},
	}, func() *serveServer { return srv })
	// Match runServe's order: only now does the server exist.
	srv = f.srv
	runs := newServeResponseRunManager()
	defer runs.Close()
	srv.responseRuns = runs
	rt, err := factory(f.ctx, serveRuntimeRequest{
		SessionID: f.parent.ID, Provider: "debug", Model: "fast", RuntimeDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if rt.spawnRunner.currentChildRunObserver() != srv.ensureChildRuns() {
		t.Fatal("runtime factory captured a nil registry before server initialization")
	}

	var observed bool
	var childResponseID string
	spawn := rt.toolMgr.GetSpawnAgentTool()
	spawn.SetEventCallback(func(callID string, event tools.SubagentEvent) {
		if event.Type != tools.SubagentEventText || observed {
			return
		}
		observed = true
		// This callback runs inside the child. Its result cannot exist yet.
		rr := httptest.NewRecorder()
		srv.handleSessionChildren(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/"+f.parent.ID+"/children", nil), f.parent.ID)
		var payload struct {
			Children []childRunProjection `json:"children"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Error(err)
			return
		}
		if rr.Code != http.StatusOK || len(payload.Children) != 1 {
			t.Errorf("live children response = %d %s", rr.Code, rr.Body.String())
			return
		}
		child := payload.Children[0]
		childResponseID = child.ResponseID
		if !child.Live || child.ParentSpawnCallID != callID || child.ParentSpawnItemID != 0 || child.ResponseID == "" {
			t.Errorf("child not linked to a standard response before result: %+v", child)
		}
		selected, err := srv.selectedWebSession(f.ctx, child.SessionID, nil)
		if err != nil || selected == nil {
			t.Errorf("live child not selectable: %v", err)
		}
	})
	output, err := spawn.Execute(f.ctx, json.RawMessage(`{"agent_name":"drill-child","prompt":"Say hello","model":"debug:fast"}`))
	if err != nil || strings.Contains(output.Content, `"error":`) {
		t.Fatalf("spawn = %s, err = %v", output.Content, err)
	}
	if !observed {
		t.Fatal("child never reached its live callback")
	}
	if childResponseID == "" {
		t.Fatal("child did not expose a response identity")
	}
	replay := httptest.NewRecorder()
	srv.handleResponseByID(replay, httptest.NewRequest(http.MethodGet, "/v1/responses/"+childResponseID+"/events?after=0", nil))
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), "event: response.created") || !strings.Contains(replay.Body.String(), "event: response.completed") {
		t.Fatalf("standard response replay = %d %s", replay.Code, replay.Body.String())
	}
	var spawnResult tools.SpawnAgentResult
	if err := json.Unmarshal([]byte(output.Content), &spawnResult); err != nil {
		t.Fatal(err)
	}
	messages, err := f.store.GetMessages(context.Background(), spawnResult.SessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, attached := srv.sessionMgr.Get(spawnResult.SessionID); attached {
		t.Fatal("completed child runtime remained attached to the session manager")
	}
	var tagged bool
	for _, message := range messages {
		if message.Role != llm.RoleUser && message.ResponseID == childResponseID {
			tagged = true
			break
		}
	}
	if !tagged {
		t.Fatalf("child transcript has no durable output tagged with response %q: %+v", childResponseID, messages)
	}
}

// TestDelegatedRunWithoutHostRegistersNothing asserts the containment property
// the whole design rests on: with no host observer installed, the CLI spawn
// path gains no registration, no intervention record and no new behavior.
func TestDelegatedRunWithoutHostRegistersNothing(t *testing.T) {
	f := newChildRunFixture(t, false)
	result := f.spawnChild(t)
	if result.SessionID == "" || result.Error != "" {
		t.Fatalf("spawn result = %+v", result)
	}
	if handle := f.srv.ensureChildRuns().lookup(result.SessionID); handle != nil {
		t.Fatal("a delegated run registered itself with no host observer installed")
	}
	if len(result.Interventions) != 0 || result.InterventionDisposition != "" || result.CancelledByUser {
		t.Fatalf("unhosted run reported intervention state: %+v", result)
	}
	child, err := f.store.Get(context.Background(), result.SessionID)
	if err != nil || child == nil || child.Status != session.StatusComplete || child.ParentID != f.parent.ID {
		t.Fatalf("child session = %+v, err = %v", child, err)
	}
}

func TestHostedChildApprovalPolicyUsesServerDefault(t *testing.T) {
	for _, defaultMode := range []tools.ApprovalMode{tools.ModePrompt, tools.ModeAuto} {
		for _, parentMode := range []tools.ApprovalMode{tools.ModePrompt, tools.ModeAuto} {
			t.Run(defaultMode.String()+"/parent-"+parentMode.String(), func(t *testing.T) {
				f := newChildRunFixture(t, true)
				t.Cleanup(f.srv.ensureResponseRuns().Close)
				f.srv.approvalDefault = defaultMode
				parent := f.env.runtime.toolMgr.ApprovalMgr
				parent.SetPolicyReviewFunc(func(context.Context, tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
					return tools.PolicyDecision{Allowed: true}, nil
				}, nil)
				parent.SetApprovalMode(parentMode)
				observed := false
				f.spawn.SetEventCallback(func(_ string, event tools.SubagentEvent) {
					if observed || event.Type != tools.SubagentEventText {
						return
					}
					observed = true
					children := f.srv.ensureChildRuns().forParent(f.parent.ID)
					if len(children) != 1 {
						t.Errorf("live children = %d, want 1", len(children))
						return
					}
					childID := children[0].childSessionID
					rr := httptest.NewRecorder()
					f.srv.handleSessionState(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/"+childID+"/state", nil), childID)
					var state struct {
						Policy map[string]any `json:"approval_policy"`
					}
					if err := json.Unmarshal(rr.Body.Bytes(), &state); err != nil || rr.Code != http.StatusOK {
						t.Errorf("session state = %d %s, err=%v", rr.Code, rr.Body.String(), err)
						return
					}
					for key, want := range map[string]any{
						"default_mode": defaultMode.String(), "requested_mode": parentMode.String(),
						"effective_mode": parentMode.String(), "guardian_available": true,
						"guardian_auto_suspended": false,
					} {
						if got := state.Policy[key]; got != want {
							t.Errorf("%s = %v, want %v", key, got, want)
						}
					}
					// Reporting Auto as the default must not pin the child's policy
					// to Auto: the parent remains authoritative while it is running.
					parent.SetApprovalMode(tools.ModePrompt)
					child, ok := f.srv.sessionMgr.Get(childID)
					if !ok || child.toolMgr == nil || child.toolMgr.ApprovalMgr == nil || child.toolMgr.ApprovalMgr.ApprovalMode() != tools.ModePrompt {
						t.Error("child stopped inheriting the parent's approval mode")
					}
				})
				if result := f.spawnChild(t); result.Error != "" || !observed {
					t.Fatalf("spawn result = %+v, observed=%v", result, observed)
				}
			})
		}
	}
}

func TestHostedChildUsesStandardSteeringAndReportsOutcome(t *testing.T) {
	f := newChildRunFixture(t, true)
	var steered bool
	result, err := f.env.runtime.spawnRunner.RunAgentWithCallback(f.ctx, "drill-child", "Say hello", 0, "spawn-steer", func(_ string, event tools.SubagentEvent) {
		if steered || event.Type != tools.SubagentEventText {
			return
		}
		handles := f.srv.ensureChildRuns().forParent(f.parent.ID)
		if len(handles) != 1 || f.srv.ensureResponseRuns().activeRun(handles[0].childSessionID) == nil {
			t.Errorf("hosted child was not attached to a response run")
			return
		}
		handle := handles[0]
		response := f.srv.ensureResponseRuns().activeRun(handle.childSessionID)
		response.mu.Lock()
		responseID, runEpoch := response.id, response.runEpoch
		response.mu.Unlock()
		body := fmt.Sprintf(`{"message":"prefer v2","delivery":"steer","client_message_id":"child-steer-1","steering_id":"child-steer-1","expected_response_id":%q,"expected_run_epoch":%d}`, responseID, runEpoch)
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+handle.childSessionID+"/steering", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		f.srv.handleSessionInterrupt(rr, req, handle.childSessionID)
		if rr.Code != http.StatusOK {
			t.Errorf("standard steering = %d %s", rr.Code, rr.Body.String())
			return
		}
		steered = true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !steered || len(result.Interventions) != 1 || result.Interventions[0] != "prefer v2" || result.InterventionDisposition != tools.InterventionConsumed {
		t.Fatalf("standard steering outcome = %+v, steered=%v", result, steered)
	}
}

func TestHostedChildStandardCancelIsDistinguishedFromParentCancel(t *testing.T) {
	f := newChildRunFixture(t, true)
	run := func(ctx context.Context, callID string, stop func(*childRunHandle)) (tools.SpawnAgentRunResult, error) {
		var stopped bool
		return f.env.runtime.spawnRunner.RunAgentWithCallback(ctx, "drill-child", "Say hello", 0, callID, func(_ string, event tools.SubagentEvent) {
			if stopped || event.Type != tools.SubagentEventText {
				return
			}
			handles := f.srv.ensureChildRuns().forParent(f.parent.ID)
			if len(handles) == 1 && f.srv.ensureResponseRuns().activeRun(handles[0].childSessionID) != nil {
				stopped = true
				stop(handles[0])
			}
		})
	}

	userResult, userErr := run(f.ctx, "spawn-user-cancel", func(handle *childRunHandle) {
		rr := httptest.NewRecorder()
		f.srv.handleResponseByID(rr, httptest.NewRequest(http.MethodPost, "/v1/responses/"+f.srv.ensureResponseRuns().activeRun(handle.childSessionID).id+"/cancel", nil))
		if rr.Code != http.StatusOK {
			t.Errorf("standard cancel = %d %s", rr.Code, rr.Body.String())
		}
	})
	if !errors.Is(userErr, context.Canceled) || !userResult.CancelledByUser {
		t.Fatalf("explicit cancel result = %+v, err=%v", userResult, userErr)
	}

	internalResult, internalErr := run(f.ctx, "spawn-internal-cancel", func(handle *childRunHandle) {
		f.srv.ensureResponseRuns().activeRun(handle.childSessionID).cancelRun()
	})
	if !errors.Is(internalErr, context.Canceled) || internalResult.CancelledByUser {
		t.Fatalf("internal cancel result = %+v, err=%v", internalResult, internalErr)
	}

	parentCtx, cancelParent := context.WithCancel(f.ctx)
	parentResult, parentErr := run(parentCtx, "spawn-parent-cancel", func(*childRunHandle) { cancelParent() })
	if !errors.Is(parentErr, context.Canceled) || parentResult.CancelledByUser {
		t.Fatalf("parent cancel result = %+v, err=%v", parentResult, parentErr)
	}
}

// A delegated transcript is controlled through its active standard response;
// starting an unrelated top-level response against it remains forbidden.
func TestResponsesRejectDelegatedSession(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Create(ctx, &session.Session{ID: "parent-rw", Provider: "debug", Model: "fast"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &session.Session{ID: "child-ro", Provider: "debug", Model: "fast", ParentID: "parent-rw", IsSubagent: true}); err != nil {
		t.Fatal(err)
	}
	// A branch carries no parent link, so it stays an ordinary conversation.
	if err := store.Create(ctx, &session.Session{ID: "branch-rw", Provider: "debug", Model: "fast"}); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store}

	for _, tc := range []struct {
		sessionID string
		delegated bool
	}{
		{sessionID: "child-ro", delegated: true},
		{sessionID: "branch-rw"},
		{sessionID: "parent-rw"},
		{sessionID: "does-not-exist"},
	} {
		got, err := srv.sessionIsDelegatedRun(ctx, tc.sessionID)
		if err != nil {
			t.Fatalf("%s: %v", tc.sessionID, err)
		}
		if got != tc.delegated {
			t.Fatalf("%s delegated = %v, want %v", tc.sessionID, got, tc.delegated)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"continue","stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(requestSessionIDHeader, "child-ro")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delegated response status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "delegated transcript") {
		t.Fatalf("rejection does not explain itself: %s", rr.Body.String())
	}

	// previous_response_id reaches a session by a different key. Drill-down makes
	// a child's durable response IDs ordinary things a client now holds, so the
	// fence has to survive a request that never names the session directly.
	srv.responseToSession.Store("resp-from-child", "child-ro")
	chained := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":"continue","stream":false,"previous_response_id":"resp-from-child"}`))
	chained.Header.Set("Content-Type", "application/json")
	chainedRR := httptest.NewRecorder()
	srv.handleResponses(chainedRR, chained)
	if chainedRR.Code != http.StatusConflict {
		t.Fatalf("chained delegated response status = %d, body = %s", chainedRR.Code, chainedRR.Body.String())
	}
	if !strings.Contains(chainedRR.Body.String(), "delegated transcript") {
		t.Fatalf("chained rejection does not explain itself: %s", chainedRR.Body.String())
	}
}

// TestChildRunStreamReportsOverflow covers the reconnect failure that matters:
// a reader outside the retained window must be told, not handed a tail with a
// silent hole in it. A fresh reader is not exempt — joining a turn that already
// wrapped the buffer is exactly how a transcript ends up looking complete while
// missing its beginning.
func TestChildTranscriptWriteEmitsStoreChange(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Create(ctx, &session.Session{ID: "feed-parent", Provider: "debug", Model: "fast"}); err != nil {
		t.Fatal(err)
	}
	child := &session.Session{ID: "feed-child", Provider: "debug", Model: "fast", ParentID: "feed-parent", IsSubagent: true}
	if err := store.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListStoreChanges(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	for _, change := range before {
		cursor = change.Sequence
	}
	if err := store.AddMessage(ctx, child.ID, session.NewMessage(child.ID, llm.UserText("child turn"), -1)); err != nil {
		t.Fatal(err)
	}
	changes, err := store.ListStoreChanges(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Kind == session.StoreChangeSessionTranscriptChanged && change.SessionID == child.ID {
			return
		}
	}
	t.Fatalf("child transcript write produced no transcript change: %+v", changes)
}

// TestChildAskUserRoutesToParentSurface covers ASK end to end at the routing
// boundary: a question raised inside a delegated run must appear where the user
// already is, be answerable there, and refuse a second answerer.
func TestChildAskUserRoutesToParentSurface(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Create(ctx, &session.Session{ID: "ask-parent", Provider: "debug", Model: "fast"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &session.Session{ID: "ask-child", Provider: "debug", Model: "fast", ParentID: "ask-parent"}); err != nil {
		t.Fatal(err)
	}
	clock := newFakeResponseRunClock()
	runCtx, runTimer := newResponseRunTimerWithClock(500*time.Millisecond, clock)
	defer runTimer.stop()
	parentRT := &serveRuntime{pauseResponseTimeout: runTimer.pause}
	srv := &serveServer{
		store:        store,
		sessionMgr:   &serveSessionManager{sessions: map[string]*serveRuntime{"ask-parent": parentRT}},
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()
	handle := srv.ensureChildRuns().ChildRunStarted(childRunInfo{
		ChildSessionID: "ask-child", ParentSessionID: "ask-parent", CallID: "spawn-1",
		Agent: "researcher", Prompt: "find the migration path",
	}).(*childRunHandle)
	if !handle.AskUserAvailable() {
		t.Fatal("a loaded parent runtime did not advertise an ask_user transport")
	}

	type outcome struct {
		answers []tools.AskUserAnswer
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		answers, err := handle.AskUser(llm.ContextWithCallID(runCtx, "call-ask"), []tools.AskUserQuestion{{
			Header: "Target", Question: "Which schema?",
			Options: []tools.AskUserOption{{Label: "v2", Description: "the new one"}},
		}})
		done <- outcome{answers, err}
	}()
	waitForServeCondition(t, 2*time.Second, func() bool {
		return len(parentRT.pendingAskUserPrompts()) == 1
	}, "child question on the parent surface")

	prompt := parentRT.pendingAskUserPrompts()[0]
	if prompt.CallID != "ask-child:call-ask" {
		t.Fatalf("prompt call id = %q, want the child-qualified id", prompt.CallID)
	}
	if prompt.Origin == nil || prompt.Origin.ChildSessionID != "ask-child" || prompt.Origin.ChildAgent != "researcher" {
		t.Fatalf("prompt does not attribute the asking subagent: %#v", prompt.Origin)
	}
	if prompt.Origin.ChildTask != "find the migration path" {
		t.Fatalf("prompt does not say what the subagent was sent to do: %#v", prompt.Origin)
	}
	// The subagent index is the other place attention has to land.
	rr := httptest.NewRecorder()
	srv.handleSessionChildren(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/ask-parent/children", nil), "ask-parent")
	var listing struct {
		Children []childRunProjection `json:"children"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Children) != 1 || !listing.Children[0].Attention || listing.Children[0].PendingAsks != 1 {
		t.Fatalf("subagent index did not raise attention for the question: %s", rr.Body.String())
	}
	if !listing.Children[0].Live {
		t.Fatalf("subagent index did not report the run as live: %s", rr.Body.String())
	}

	answerBody := `{"call_id":"ask-child:call-ask","answers":[{"question_index":0,"header":"Target","selected":"v2"}]}`
	answerReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/ask-parent/ask_user", strings.NewReader(answerBody))
	answerReq.Header.Set("Content-Type", "application/json")
	answerRR := httptest.NewRecorder()
	srv.handleSessionAskUser(answerRR, answerReq, "ask-parent")
	if answerRR.Code != http.StatusOK {
		t.Fatalf("answer status = %d, body = %s", answerRR.Code, answerRR.Body.String())
	}

	select {
	case got := <-done:
		if got.err != nil || len(got.answers) != 1 || got.answers[0].Selected != "v2" {
			t.Fatalf("child received %#v, err = %v", got.answers, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never reached the asking subagent")
	}

	secondRR := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/ask-parent/ask_user", strings.NewReader(answerBody))
	secondReq.Header.Set("Content-Type", "application/json")
	srv.handleSessionAskUser(secondRR, secondReq, "ask-parent")
	if secondRR.Code != http.StatusConflict {
		t.Fatalf("second answerer status = %d, want 409 (body %s)", secondRR.Code, secondRR.Body.String())
	}
}

// A delegated run keeps the caller's deadline as well as cancellation and
// context values. Pausing the response inactivity timer must not extend it.
func TestHostedChildTimerPreservesParentDeadline(t *testing.T) {
	type key struct{}
	deadline := time.Unix(1, 0)
	parent, cancel := context.WithDeadline(context.WithValue(context.Background(), key{}, "parent"), deadline)
	defer cancel()
	ctx, timer := newResponseRunTimerFrom(parent, time.Minute, realResponseRunClock{})
	defer timer.stop()
	if ctx.Err() != context.DeadlineExceeded || ctx.Value(key{}) != "parent" {
		t.Fatalf("parent context lost: err=%v value=%v", ctx.Err(), ctx.Value(key{}))
	}
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatalf("deadline=%v, present=%v", got, ok)
	}
}
