package serve

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// barrierGrandchildRunner stands in for the child's own children. It is wired
// into the spawn tool through WireSpawn so the barrier runs against real spawn
// bookkeeping instead of a mocked barrier.
type barrierGrandchildRunner struct {
	store  session.AgentRunStore
	output string
	// entered is closed once when the first run starts; release, when set,
	// holds every run open until the test closes it.
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once

	mu    sync.Mutex
	calls int
}

func (r *barrierGrandchildRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithCallback(ctx, name, prompt, depth, "", nil)
}

func (r *barrierGrandchildRunner) RunAgentWithCallback(ctx context.Context, name, prompt string, depth int, _ string, _ tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	r.mu.Lock()
	r.calls++
	entered, release := r.entered, r.release
	r.mu.Unlock()
	if entered != nil {
		r.enteredOnce.Do(func() { close(entered) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return tools.SpawnAgentRunResult{}, ctx.Err()
		}
	}
	return tools.SpawnAgentRunResult{Output: r.output}, nil
}

func (r *barrierGrandchildRunner) AgentRunStore() session.AgentRunStore { return r.store }

func (r *barrierGrandchildRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// childBarrierTestSeq keeps session identities unique per test: agent entries
// live in a process-global map that outlives one test's store.
var childBarrierTestSeq atomic.Int64

type childBarrierFixture struct {
	store      *session.SQLiteStore
	cfg        *config.Config
	parent     *session.Session
	childID    string
	mock       *mockprovider.MockProvider
	grandchild *barrierGrandchildRunner
	runner     *cmdRunner
}

func newChildBarrierFixture(t *testing.T, mock *mockprovider.MockProvider, grandchild *barrierGrandchildRunner) *childBarrierFixture {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seq := childBarrierTestSeq.Add(1)
	parent := &session.Session{ID: fmt.Sprintf("barrier-parent-%d", seq), Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	childID := fmt.Sprintf("barrier-child-%d", seq)
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers:       map[string]config.ProviderConfig{"mock": {Model: "mock-model"}},
		Sessions:        config.SessionsConfig{Enabled: true, Path: dbPath},
	}
	grandchild.store = store
	runner := newCmdRunner(cfg, cmdRunnerOptions{
		Store: store, Tools: "spawn_agent", ToolsSet: true,
		WireSpawn: func(_ *config.Config, mgr *tools.ToolManager, _ bool) error {
			spawn := mgr.GetSpawnAgentTool()
			if spawn == nil {
				return errors.New("spawn tool missing")
			}
			spawn.SetRunner(grandchild)
			return nil
		},
	}).(*cmdRunner)
	return &childBarrierFixture{store: store, cfg: cfg, parent: parent, childID: childID, mock: mock, grandchild: grandchild, runner: runner}
}

// runChild starts one delegated child run on the fixture.
func (f *childBarrierFixture) runChild(t *testing.T, maxTurns int) (runpkg.Result, error) {
	t.Helper()
	return f.runChildContext(context.Background(), t, maxTurns)
}

func (f *childBarrierFixture) runChildContext(ctx context.Context, t *testing.T, maxTurns int) (runpkg.Result, error) {
	t.Helper()
	return f.runner.Run(ctx, runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		SessionID:        f.childID,
		ParentSessionID:  f.parent.ID,
		IsSubagent:       true,
		Persist:          true,
		Cwd:              t.TempDir(),
		Prompt:           "do the delegated task",
		ProviderInstance: f.mock,
		MaxTurns:         maxTurns,
		MaxTurnsSet:      true,
	}, eventSinkFunc(func(core.Event) {}))
}

// requestWithLifecycleNotice returns the recorded request that carried the
// host-authored lifecycle notice.
func requestWithLifecycleNotice(mock *mockprovider.MockProvider) (core.Request, bool) {
	for _, request := range mock.RecordedRequests() {
		for _, msg := range request.Messages {
			if strings.Contains(core.MessageText(msg), "All subagents for this session have finished") {
				return request, true
			}
		}
	}
	return core.Request{}, false
}

func agentRunRecord(t *testing.T, store session.Store, id string) session.AgentRun {
	t.Helper()
	record, err := session.AsAgentRunStore(store).GetAgentRun(context.Background(), id)
	if err != nil {
		t.Fatalf("agent run %s: %v", id, err)
	}
	return record
}

// A child that forgets wait_agent must stay running until its grandchild
// finishes, receive a lifecycle turn, and report only that final turn.
func TestChildBarrierWaitsForGrandchildAndReportsFinalResponse(t *testing.T) {
	mock := mockprovider.NewMockProvider("mock").
		AddToolCall("call-spawn", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "grandchild work", "wait": 0}).
		AddTextResponse("child: spawned, moving on without waiting").
		AddTextResponse("final report: grandchild delivered")
	grandchild := &barrierGrandchildRunner{
		entered: make(chan struct{}), release: make(chan struct{}),
		output: "grandchild secret output",
	}
	f := newChildBarrierFixture(t, mock, grandchild)

	type outcome struct {
		result runpkg.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := f.runChild(t, 8)
		done <- outcome{result, err}
	}()

	<-grandchild.entered
	// The child's own turn is finished; the run must still be alive because a
	// grandchild it never waited for is running.
	select {
	case got := <-done:
		t.Fatalf("child run returned before its grandchild finished: %+v, %v", got.result, got.err)
	case <-time.After(150 * time.Millisecond):
	}
	close(grandchild.release)

	got := <-done
	if got.err != nil {
		t.Fatalf("child run error = %v", got.err)
	}
	if !strings.Contains(got.result.Response, "final report: grandchild delivered") {
		t.Fatalf("child output = %q, want the lifecycle turn's report", got.result.Response)
	}
	if strings.Contains(got.result.Response, "moving on without waiting") {
		t.Fatalf("child output concatenated turns: %q", got.result.Response)
	}
	record := agentRunRecord(t, f.store, grandchildRecordID(t, f.store, f.childID))
	if !record.CollectedAt.IsZero() {
		t.Fatalf("barrier turn collected a result it never delivered: %+v", record)
	}
	if record.NotifiedAt.IsZero() {
		t.Fatalf("lifecycle turn did not acknowledge the delivered result: %+v", record)
	}
	notice, ok := requestWithLifecycleNotice(mock)
	if !ok {
		t.Fatal("the child never received a lifecycle notice")
	}
	noticeText := ""
	for _, msg := range notice.Messages {
		noticeText += core.MessageText(msg) + "\n"
	}
	if !strings.Contains(noticeText, `"agent_name":"developer"`) || !strings.Contains(noticeText, `"status":"completed"`) {
		t.Fatalf("lifecycle notice lacks child metadata: %s", noticeText)
	}
	if strings.Contains(noticeText, "grandchild secret output") {
		t.Fatal("lifecycle notice embedded child output")
	}
}

func grandchildRecordID(t *testing.T, store session.Store, parent string) string {
	t.Helper()
	runs, err := session.AsAgentRunStore(store).ListAgentRuns(context.Background(), parent)
	if err != nil || len(runs) != 1 {
		t.Fatalf("agent runs for %s = %+v, err = %v", parent, runs, err)
	}
	return runs[0].ID
}

// A child that collected its grandchild's result owes no further turn.
func TestChildBarrierSkipsLifecycleTurnWhenResultsCollected(t *testing.T) {
	mock := mockprovider.NewMockProvider("mock").
		AddToolCall("call-spawn", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "grandchild work"}).
		AddTextResponse("child: collected, done")
	grandchild := &barrierGrandchildRunner{output: "grandchild output"}
	f := newChildBarrierFixture(t, mock, grandchild)

	result, err := f.runChild(t, 8)
	if err != nil {
		t.Fatalf("child run error = %v", err)
	}
	if !strings.Contains(result.Response, "child: collected, done") {
		t.Fatalf("child output = %q", result.Response)
	}
	if requests := len(mock.RecordedRequests()); requests != 2 {
		t.Fatalf("provider turns = %d, want 2 (no lifecycle turn for a collected result)", requests)
	}
	if _, ok := requestWithLifecycleNotice(mock); ok {
		t.Fatal("a collected result still produced a lifecycle notice")
	}
	record := agentRunRecord(t, f.store, grandchildRecordID(t, f.store, f.childID))
	if record.CollectedAt.IsZero() {
		t.Fatalf("spawn result was not recorded as collected: %+v", record)
	}
}

// Once the ordinary allowance is exhausted, only the bounded lifecycle reserve
// remains: the child still receives and acknowledges the notice, but may not
// create work from it.
func TestChildBarrierReserveTurnCannotSpawn(t *testing.T) {
	mock := mockprovider.NewMockProvider("mock").
		AddToolCall("call-spawn", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "grandchild work", "wait": 0}).
		AddTextResponse("child: spawned, out of turns").
		AddToolCall("call-spawn-again", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "more work", "wait": 0}).
		AddTextResponse("reserve report: results acknowledged")
	grandchild := &barrierGrandchildRunner{output: "grandchild output"}
	f := newChildBarrierFixture(t, mock, grandchild)

	// Two provider turns consume the whole allowance: the barrier turn runs on
	// the lifecycle reserve.
	result, err := f.runChild(t, 2)
	if err != nil {
		t.Fatalf("child run error = %v", err)
	}
	if !strings.Contains(result.Response, "reserve report: results acknowledged") {
		t.Fatalf("child output = %q, want the reserve turn's report", result.Response)
	}
	if calls := grandchild.callCount(); calls != 1 {
		t.Fatalf("grandchild runs = %d, want 1: a lifecycle reserve turn spawned work", calls)
	}
	if record := agentRunRecord(t, f.store, grandchildRecordID(t, f.store, f.childID)); record.NotifiedAt.IsZero() {
		t.Fatalf("reserve turn did not acknowledge the delivered result: %+v", record)
	}
	if _, ok := requestWithLifecycleNotice(mock); !ok {
		t.Fatal("the reserve turn never received a lifecycle notice")
	}
}

// Every child run owns the barrier, including a resumed child (continue_agent):
// results its earlier run left uncollected are delivered on the resume.
func TestResumedChildBarrierDeliversPendingResults(t *testing.T) {
	mock := mockprovider.NewMockProvider("mock").
		AddTextResponse("resumed child: continuing the task").
		AddTextResponse("resumed final report: delivered pending result")
	grandchild := &barrierGrandchildRunner{output: "grandchild output"}
	f := newChildBarrierFixture(t, mock, grandchild)
	ctx := context.Background()
	if err := f.store.Create(ctx, &session.Session{ID: f.childID, ParentID: f.parent.ID, IsSubagent: true, Provider: "mock", Model: "mock-model", Mode: session.ModeChat}); err != nil {
		t.Fatal(err)
	}
	pending := session.AgentRun{
		ID: f.childID + "-pending", ParentSessionID: f.childID, AgentName: "developer", Prompt: "earlier work",
		Status: "completed", StopReason: "completed", OwnerInstanceID: "another-host", RunGeneration: 1,
		UpdatedAt: time.Now(),
	}
	if err := session.AsAgentRunStore(f.store).PutAgentRun(ctx, pending); err != nil {
		t.Fatal(err)
	}

	result, err := f.runner.Run(ctx, runpkg.Request{
		Platform: runpkg.PlatformConsole, SessionID: f.childID, ParentSessionID: f.parent.ID,
		IsSubagent: true, Persist: true, Stateful: true, Resume: true,
		Messages: []core.Message{core.UserText("please continue")}, Cwd: t.TempDir(),
		ProviderInstance: mock, MaxTurns: 8, MaxTurnsSet: true,
	}, eventSinkFunc(func(core.Event) {}))
	if err != nil {
		t.Fatalf("resumed child run error = %v", err)
	}
	if !strings.Contains(result.Response, "resumed final report: delivered pending result") {
		t.Fatalf("resumed child output = %q, want the lifecycle turn's report", result.Response)
	}
	if record := agentRunRecord(t, f.store, pending.ID); record.NotifiedAt.IsZero() {
		t.Fatalf("resumed child barrier did not acknowledge the pending result: %+v", record)
	}
}

// A lifecycle turn that fails transiently is retried while the child is live:
// undelivered results have no other owner once the child ends.
func TestChildBarrierRetriesFailedLifecycleTurn(t *testing.T) {
	previousBackoff := childLifecycleRetryBackoff
	childLifecycleRetryBackoff = []time.Duration{time.Millisecond}
	defer func() { childLifecycleRetryBackoff = previousBackoff }()

	mock := mockprovider.NewMockProvider("mock").
		AddToolCall("call-spawn", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "grandchild work", "wait": 0}).
		AddTextResponse("child: spawned, moving on").
		AddError(errors.New("provider unavailable")).
		AddTextResponse("final report: delivered after retry")
	grandchild := &barrierGrandchildRunner{output: "grandchild output"}
	f := newChildBarrierFixture(t, mock, grandchild)

	result, err := f.runChild(t, 8)
	if err != nil {
		t.Fatalf("child run error = %v", err)
	}
	if !strings.Contains(result.Response, "delivered after retry") {
		t.Fatalf("child output = %q, want the retried lifecycle turn's report", result.Response)
	}
	record := agentRunRecord(t, f.store, grandchildRecordID(t, f.store, f.childID))
	if record.NotifiedAt.IsZero() {
		t.Fatalf("result was delivered but never acknowledged: %+v", record)
	}
}

// A lifecycle turn that spends its whole allowance still delivered the notice:
// the offered results are acknowledged, the typed limit is preserved, and a
// child that turn started still receives its own lifecycle wake.
func TestChildBarrierLimitTurnIsAcknowledgedAndReserveWakesNewChild(t *testing.T) {
	mock := mockprovider.NewMockProvider("mock").
		AddToolCall("call-spawn-a", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "first grandchild", "wait": 0}).
		AddTextResponse("child: spawned A without waiting").
		AddToolCall("call-spawn-c", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "second grandchild", "wait": 0}).
		AddTextResponse("reserve report: C delivered")
	grandchild := &barrierGrandchildRunner{output: "grandchild output"}
	f := newChildBarrierFixture(t, mock, grandchild)

	// The child's own turn spends two of three turns, so its first lifecycle
	// turn runs on the ordinary allowance and ends on the typed limit.
	result, err := f.runChild(t, 3)
	var limit *engine.MaxTurnsExceededError
	if !errors.As(err, &limit) {
		t.Fatalf("child run error = %v, want the preserved typed turn limit", err)
	}
	if !strings.Contains(result.Response, "reserve report: C delivered") {
		t.Fatalf("child output = %q, want the reserve turn's report", result.Response)
	}
	if calls := grandchild.callCount(); calls != 2 {
		t.Fatalf("grandchild runs = %d, want 2 (the limit turn's child must still be woken)", calls)
	}
	runs, err := session.AsAgentRunStore(f.store).ListAgentRuns(context.Background(), f.childID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("agent runs = %+v, err = %v", runs, err)
	}
	for _, run := range runs {
		if run.NotifiedAt.IsZero() {
			t.Fatalf("grandchild %s was never acknowledged: %+v", run.ID, run)
		}
	}
	notices := 0
	for _, request := range mock.RecordedRequests() {
		for _, msg := range request.Messages {
			if strings.Contains(core.MessageText(msg), "All subagents for this session have finished") {
				notices++
				break
			}
		}
	}
	if notices != 2 {
		t.Fatalf("lifecycle notices = %d, want one per delivered generation", notices)
	}
}

// A stop while the child waits for its children must propagate as a stop, and
// no lifecycle turn may run after it.
func TestChildBarrierCancelStopsChildWithoutLifecycleTurn(t *testing.T) {
	mock := mockprovider.NewMockProvider("mock").
		AddToolCall("call-spawn", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "grandchild work", "wait": 0}).
		AddTextResponse("child: spawned, moving on").
		AddTextResponse("this lifecycle turn must never run")
	grandchild := &barrierGrandchildRunner{
		entered: make(chan struct{}), release: make(chan struct{}),
		output: "grandchild output",
	}
	f := newChildBarrierFixture(t, mock, grandchild)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		result runpkg.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := f.runChildContext(ctx, t, 8)
		done <- outcome{result, err}
	}()
	<-grandchild.entered
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancelled child run error = %v, want context.Canceled", got.err)
		}
		if strings.Contains(got.result.Response, "must never run") {
			t.Fatalf("a lifecycle turn ran after cancel: %q", got.result.Response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled child run did not stop")
	}
	if _, ok := requestWithLifecycleNotice(mock); ok {
		t.Fatal("a stopped child received a lifecycle turn")
	}
}

// A lifecycle turn with ordinary allowance left may still delegate, and the
// next pass delivers that new child's result.
func TestChildBarrierBudgetTurnCanSpawnAgain(t *testing.T) {
	mock := mockprovider.NewMockProvider("mock").
		AddToolCall("call-spawn", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "grandchild work", "wait": 0}).
		AddTextResponse("child: spawned, waiting on results").
		AddToolCall("call-spawn-more", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "follow-up work", "wait": 0}).
		AddTextResponse("barrier: delegated follow-up").
		AddTextResponse("barrier: every child reported")
	grandchild := &barrierGrandchildRunner{output: "grandchild output"}
	f := newChildBarrierFixture(t, mock, grandchild)

	result, err := f.runChild(t, 12)
	if err != nil {
		t.Fatalf("child run error = %v", err)
	}
	if calls := grandchild.callCount(); calls != 2 {
		t.Fatalf("grandchild runs = %d, want 2: a lifecycle turn with budget could not delegate", calls)
	}
	if !strings.Contains(result.Response, "barrier: every child reported") {
		t.Fatalf("child output = %q, want the last lifecycle turn's report", result.Response)
	}
	runs, err := session.AsAgentRunStore(f.store).ListAgentRuns(context.Background(), f.childID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("agent runs = %+v, want both grandchildren, err = %v", runs, err)
	}
	for _, run := range runs {
		if run.NotifiedAt.IsZero() {
			t.Fatalf("grandchild %s was never acknowledged: %+v", run.ID, run)
		}
	}
}

// A child that captured structured output before its lifecycle turn must not
// have that stale capture reported instead of the lifecycle turn's own output.
func TestHostedChildBarrierResetsStructuredOutputCapture(t *testing.T) {
	f := newChildRunFixture(t, true)
	ctx := context.Background()
	childID := fmt.Sprintf("barrier-output-child-%d", childBarrierTestSeq.Add(1))
	if err := f.store.Create(ctx, &session.Session{ID: childID, ParentID: f.parent.ID, IsSubagent: true, Provider: "debug", Model: "fast", Mode: session.ModeChat}); err != nil {
		t.Fatal(err)
	}
	pending := session.AgentRun{
		ID: childID + "-pending", ParentSessionID: childID, AgentName: "developer", Prompt: "earlier work",
		Status: "completed", StopReason: "completed", OwnerInstanceID: "another-host", RunGeneration: 1,
		UpdatedAt: time.Now(),
	}
	if err := session.AsAgentRunStore(f.store).PutAgentRun(ctx, pending); err != nil {
		t.Fatal(err)
	}

	handle := f.srv.ensureChildRuns().ChildRunStarted(childRunInfo{
		ChildSessionID: childID, ParentSessionID: f.parent.ID, CallID: "spawn-struct", Agent: "drill-child", Prompt: "work",
	}).(*childRunHandle)
	sink := &barrierChildSink{handle: handle}
	runner := newCmdRunner(f.env.cfg, cmdRunnerOptions{Store: f.store, Tools: "spawn_agent", ToolsSet: true}).(*cmdRunner)
	outputTool := &runpkg.HostOutputTool{Name: "set_output", Param: "content", Description: "Report the final result", Schema: map[string]any{"type": "object"}}
	env, err := runner.prepare(ctx, runpkg.Request{
		Platform: runpkg.PlatformWeb, SessionID: childID, ParentSessionID: f.parent.ID, IsSubagent: true, Persist: true,
		Cwd: t.TempDir(), Provider: "debug", Model: "fast", AgentName: "drill-child",
		// The debug provider's DSL turns this prompt into a call of the agent's
		// finishing output tool, exactly as a real child captures structured output.
		Prompt:         "call set_output content=stale_structured_report",
		HostOutputTool: outputTool,
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()

	result, err := handle.Execute(ctx, env, sink)
	if err != nil {
		t.Fatalf("hosted child run error = %v", err)
	}
	agentCfg := &agents.Agent{OutputTool: agents.OutputToolConfig{Name: "set_output", Param: "content"}}
	reported, completionErr := completeChildAgent(agentCfg, result, "", t.TempDir(), false)
	if completionErr != nil {
		t.Fatalf("completion hook error = %v", completionErr)
	}
	if strings.Contains(reported, "stale_structured_report") {
		t.Fatalf("lifecycle turn reported the previous turn's capture: %q", reported)
	}
	if !strings.Contains(reported, "Debug Provider Output") {
		t.Fatalf("reported child output = %q, want the lifecycle turn's own report", reported)
	}
	if record := agentRunRecord(t, f.store, pending.ID); record.NotifiedAt.IsZero() {
		t.Fatalf("hosted barrier turn did not acknowledge the pending result: %+v", record)
	}
}

type barrierChildSink struct {
	handle     childRunSession
	boundaries int
	events     int
}

func (s *barrierChildSink) Event(core.Event) { s.events++ }
func (s *barrierChildSink) ChildRunSession() childRunSession {
	return s.handle
}
func (s *barrierChildSink) BeginChildTurn() { s.boundaries++ }

// The hosted web child must keep the borrowed runtime alive across its
// lifecycle turns and deliver results that were still pending when it started.
func TestHostedChildBarrierRetainsRuntimeAndDeliversPendingResults(t *testing.T) {
	f := newChildRunFixture(t, true)
	ctx := context.Background()
	childID := fmt.Sprintf("barrier-hosted-child-%d", childBarrierTestSeq.Add(1))
	if err := f.store.Create(ctx, &session.Session{ID: childID, ParentID: f.parent.ID, IsSubagent: true, Provider: "debug", Model: "fast", Mode: session.ModeChat}); err != nil {
		t.Fatal(err)
	}
	// A completed run from a previous generation whose result the child never
	// saw: the barrier owes it a lifecycle turn.
	pending := session.AgentRun{
		ID: childID + "-pending", ParentSessionID: childID, AgentName: "developer", Prompt: "earlier work",
		Status: "completed", StopReason: "completed", OwnerInstanceID: "another-host", RunGeneration: 1,
		UpdatedAt: time.Now(),
	}
	if err := session.AsAgentRunStore(f.store).PutAgentRun(ctx, pending); err != nil {
		t.Fatal(err)
	}

	handle := f.srv.ensureChildRuns().ChildRunStarted(childRunInfo{
		ChildSessionID: childID, ParentSessionID: f.parent.ID, CallID: "spawn-hosted", Agent: "developer", Prompt: "work",
	}).(*childRunHandle)
	sink := &barrierChildSink{handle: handle}
	runner := newCmdRunner(f.env.cfg, cmdRunnerOptions{Store: f.store, Tools: "spawn_agent", ToolsSet: true}).(*cmdRunner)
	env, err := runner.prepare(ctx, runpkg.Request{
		Platform: runpkg.PlatformWeb, SessionID: childID, ParentSessionID: f.parent.ID, IsSubagent: true,
		Persist: true, Cwd: t.TempDir(), Provider: "debug", Model: "fast",
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()

	var closed atomic.Int32
	previousHook := serveRuntimeClosedHook
	serveRuntimeClosedHook = func(rt *serveRuntime) {
		if rt == env.runtime {
			closed.Add(1)
		}
	}
	defer func() { serveRuntimeClosedHook = previousHook }()

	result, err := handle.Execute(ctx, env, sink)
	if err != nil {
		t.Fatalf("hosted child run error = %v", err)
	}
	if env.runtime == nil {
		t.Fatal("hosted child dropped its runtime between turns")
	}
	if closed.Load() != 0 {
		t.Fatal("hosted child runtime was closed before its lifecycle turns ran")
	}
	if result.Response == "" {
		t.Fatal("hosted child produced no response")
	}
	if sink.boundaries != 1 {
		t.Fatalf("lifecycle turn boundaries = %d, want 1", sink.boundaries)
	}
	if sink.events == 0 {
		t.Fatal("lifecycle turn produced no events for the host")
	}
	record := agentRunRecord(t, f.store, pending.ID)
	if record.NotifiedAt.IsZero() {
		t.Fatalf("hosted barrier turn did not acknowledge the pending result: %+v", record)
	}
	messages, err := f.store.GetMessages(ctx, childID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	assistantTurns, noticeHidden := 0, true
	for _, message := range messages {
		if message.Role == core.RoleAssistant {
			assistantTurns++
		}
		if strings.Contains(message.TextContent, "All subagents for this session have finished") {
			noticeHidden = false
		}
	}
	if assistantTurns < 2 {
		t.Fatalf("hosted child transcript has %d assistant turns, want the child's own turn and its lifecycle turn: %+v", assistantTurns, messages)
	}
	if !noticeHidden {
		t.Fatal("the host-authored lifecycle notice leaked into the child transcript")
	}
}
