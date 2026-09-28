package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type lifecycleRunner struct {
	entered     chan string
	release     chan struct{}
	mu          sync.Mutex
	calls       int
	resumed     int
	steered     string
	disposition string
}

func (r *lifecycleRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (SpawnAgentRunResult, error) {
	return r.RunAgentWithCallback(ctx, name, prompt, depth, "", nil)
}
func (r *lifecycleRunner) RunAgentWithCallback(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	return r.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, id, cb, SpawnAgentRunOptions{})
}
func (r *lifecycleRunner) RunAgentWithOptions(ctx context.Context, name, prompt string, depth int, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	return r.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, "", nil, opts)
}
func (r *lifecycleRunner) RunAgentWithCallbackAndOptions(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	r.entered <- opts.ChildSessionID
	select {
	case <-r.release:
		return SpawnAgentRunResult{Output: "done", SessionID: opts.ChildSessionID}, nil
	case <-ctx.Done():
		return SpawnAgentRunResult{SessionID: opts.ChildSessionID}, ctx.Err()
	}
}
func (r *lifecycleRunner) ContinueAgent(_ context.Context, id, name, instructions string, _ int, _ string, _ SpawnAgentRunOptions, _ SubagentEventCallback) (SpawnAgentRunResult, error) {
	r.mu.Lock()
	r.resumed++
	r.mu.Unlock()
	return SpawnAgentRunResult{Output: "resumed", SessionID: id}, nil
}
func (r *lifecycleRunner) SteerAgent(_ string, instructions string) (string, string) {
	r.mu.Lock()
	r.steered = instructions
	r.mu.Unlock()
	if r.disposition != "" {
		return "steer-id", r.disposition
	}
	return "steer-id", "queued"
}

func lifecycleCall(t *testing.T, tool llm.Tool, ctx context.Context, args string) llm.ToolOutput {
	t.Helper()
	out, err := tool.Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func lifecycleResult(t *testing.T, out llm.ToolOutput) SpawnAgentResult {
	t.Helper()
	var result SpawnAgentResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

type eventBarrierRunner struct {
	emit    chan SubagentEventCallback
	release chan struct{}
}

func (r *eventBarrierRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (SpawnAgentRunResult, error) {
	return r.RunAgentWithCallback(ctx, name, prompt, depth, "", nil)
}
func (r *eventBarrierRunner) RunAgentWithCallback(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	r.emit <- cb
	<-r.release
	return SpawnAgentRunResult{}, nil
}

type parentSessionRunner struct{ *lifecycleRunner }

func (*parentSessionRunner) ParentAgentSessionID() string { return "shared-parent" }

type usageTurnRunner struct{}

func (usageTurnRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (SpawnAgentRunResult, error) {
	return SpawnAgentRunResult{}, nil
}
func (usageTurnRunner) RunAgentWithCallback(_ context.Context, _, _ string, _ int, id string, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	cb(id, SubagentEvent{Type: SubagentEventUsage}) // compaction billing
	cb(id+"/nested", SubagentEvent{Type: SubagentEventUsage, CountsTurn: true})
	cb(id, SubagentEvent{Type: SubagentEventUsage, CountsTurn: true})
	return SpawnAgentRunResult{}, nil
}

func TestAgentLifecycleCountsOnlyChildModelTurns(t *testing.T) {
	m := newAgentManager(SpawnConfig{MaxParallel: 1})
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	e, err := m.start(ctx, "developer", "work", "", "call", nil, nil, usageTurnRunner{}, 0, false, "", session.AgentRun{})
	if err != nil {
		t.Fatal(err)
	}
	<-e.done
	record, _, err := m.get(ctx, e.record.ID, "parent")
	if err != nil || record.TurnsUsed != 1 {
		t.Fatalf("turns with compaction and nested agent = %d, %v", record.TurnsUsed, err)
	}
}

func TestAgentControlRunnerReplacementIsSynchronized(t *testing.T) {
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1}, 0)
	runner := &parentSessionRunner{lifecycleRunner: &lifecycleRunner{}}
	spawn.SetRunner(runner)
	control := &agentControlTool{name: ListAgentsToolName, spawn: spawn}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			spawn.SetRunner(runner)
		}
	}()
	for i := 0; i < 100; i++ {
		out := lifecycleCall(t, control, context.Background(), `{}`)
		if out.Content != "[]" {
			t.Fatalf("list = %s", out.Content)
		}
	}
	<-done
}

func TestAgentCallbackDetachWaitsForInFlightDelivery(t *testing.T) {
	runner := &eventBarrierRunner{emit: make(chan SubagentEventCallback, 1), release: make(chan struct{})}
	m := newAgentManager(SpawnConfig{MaxParallel: 1})
	entered := make(chan struct{})
	unblock := make(chan struct{})
	entry, err := m.start(llm.ContextWithSessionID(context.Background(), "parent"), "developer", "work", "", "call", func(string, SubagentEvent) {
		close(entered)
		<-unblock
	}, nil, runner, 0, false, "", session.AgentRun{})
	if err != nil {
		t.Fatal(err)
	}
	cb := <-runner.emit
	delivered := make(chan struct{})
	go func() { cb("call", SubagentEvent{Type: SubagentEventText}); close(delivered) }()
	<-entered
	detached := make(chan struct{})
	go func() { m.detach(entry, entry.initial); close(detached) }()
	select {
	case <-detached:
		t.Fatal("detach returned while callback was executing")
	default:
	}
	close(unblock)
	<-delivered
	<-detached
	cb("call", SubagentEvent{Type: SubagentEventText}) // must not reach the old callback
	close(runner.release)
	<-entry.done
}

func TestAgentLifecycleCrossManagerWait(t *testing.T) {
	runner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	first := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	first.SetRunner(runner)
	second := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	otherRunner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	second.SetRunner(otherRunner)
	ctx := llm.ContextWithSessionID(context.Background(), "shared-parent")
	spawned := lifecycleResult(t, lifecycleCall(t, first, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	<-runner.entered
	wait := &agentControlTool{name: WaitAgentToolName, spawn: second}
	result := lifecycleCall(t, wait, ctx, `{"agent_ids":["`+spawned.AgentID+`"],"max_wait":0}`)
	if !strings.Contains(result.Content, `"status":"running"`) {
		t.Fatalf("cross-manager wait = %s", result.Content)
	}
	control := &agentControlTool{name: ContinueAgentToolName, spawn: second}
	steered := lifecycleCall(t, control, ctx, `{"agent_id":"`+spawned.AgentID+`","instructions":"owner only"}`)
	if !strings.Contains(steered.Content, `"intervention_disposition":"queued"`) {
		t.Fatalf("cross-manager steering = %s", steered.Content)
	}
	runner.mu.Lock()
	ownerInstruction := runner.steered
	runner.mu.Unlock()
	if ownerInstruction != "owner only" || otherRunner.steered != "" {
		t.Fatalf("steering routed to wrong runner: owner=%q caller=%q", ownerInstruction, otherRunner.steered)
	}
	entry, ok := processAgentEntries.Load(spawned.AgentID)
	if !ok {
		t.Fatal("missing live agent")
	}
	runnerEntry := entry.(*agentEntry)
	runnerEntry.manager.mu.Lock()
	runnerEntry.record.Status = "awaiting_approval"
	runnerEntry.manager.mu.Unlock()
	steered = lifecycleCall(t, control, ctx, `{"agent_id":"`+spawned.AgentID+`","instructions":"approval note"}`)
	if !strings.Contains(steered.Content, `"intervention_disposition":"queued"`) {
		t.Fatalf("approval steering = %s", steered.Content)
	}
	close(runner.release)
	result = lifecycleCall(t, wait, ctx, `{"agent_ids":["`+spawned.AgentID+`"],"max_wait":1}`)
	if !strings.Contains(result.Content, `"status":"completed"`) {
		t.Fatalf("cross-manager completion = %s", result.Content)
	}
}

func TestAgentLifecycleDetachQueueCancelResume(t *testing.T) {
	runner := &lifecycleRunner{entered: make(chan string, 2), release: make(chan struct{})}
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	spawn.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "parent-one")
	first := lifecycleResult(t, lifecycleCall(t, spawn, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	if first.AgentID == "" || (first.Status != "queued" && first.Status != "running") {
		t.Fatalf("first spawn = %+v", first)
	}
	select {
	case <-runner.entered:
	case <-time.After(time.Second):
		t.Fatal("first never started")
	}
	second := lifecycleResult(t, lifecycleCall(t, spawn, ctx, `{"agent_name":"developer","prompt":"another","wait":0}`))
	if second.Status != "queued" {
		t.Fatalf("second spawn = %+v", second)
	}
	cancelTool := &agentControlTool{name: CancelAgentToolName, spawn: spawn}
	cancelled := lifecycleResult(t, lifecycleCall(t, cancelTool, ctx, `{"agent_id":"`+second.AgentID+`"}`))
	if cancelled.Status != "cancelled" {
		t.Fatalf("queued cancel = %+v", cancelled)
	}
	runner.mu.Lock()
	calls := runner.calls
	runner.mu.Unlock()
	if calls != 1 {
		t.Fatalf("queued agent ran (%d calls)", calls)
	}
	steer := &agentControlTool{name: ContinueAgentToolName, spawn: spawn}
	output := lifecycleCall(t, steer, ctx, `{"agent_id":"`+first.AgentID+`","instructions":"focus on tests"}`)
	if !strings.Contains(output.Content, `"intervention_disposition":"queued"`) || !strings.Contains(output.Content, `"resumable":false`) || !strings.Contains(output.Content, `"next":"wait_agent(`) {
		t.Fatalf("steering = %s", output.Content)
	}
	runner.mu.Lock()
	runner.disposition = "undelivered"
	runner.mu.Unlock()
	output = lifecycleCall(t, steer, ctx, `{"agent_id":"`+first.AgentID+`","instructions":"retry me"}`)
	if !strings.Contains(output.Content, `"resumable":false`) || !strings.Contains(output.Content, `continue_agent(`) || !strings.Contains(output.Content, `retry me`) {
		t.Fatalf("undelivered steering = %s", output.Content)
	}
	close(runner.release)
	wait := &agentControlTool{name: WaitAgentToolName, spawn: spawn}
	for i := 0; i < 2; i++ {
		output = lifecycleCall(t, wait, ctx, `{"agent_ids":["`+first.AgentID+`"],"max_wait":1}`)
		if !strings.Contains(output.Content, `"status":"completed"`) {
			t.Fatalf("wait %d = %s", i, output.Content)
		}
	}
	resumed := lifecycleResult(t, lifecycleCall(t, steer, ctx, `{"agent_id":"`+first.AgentID+`","instructions":"review tests","wait":1}`))
	if resumed.Status != "completed" || resumed.Output != "resumed" {
		t.Fatalf("resumed = %+v", resumed)
	}
	other := llm.ContextWithSessionID(context.Background(), "parent-two")
	denied := lifecycleCall(t, wait, other, `{"agent_ids":["`+first.AgentID+`"]}`)
	if !strings.Contains(denied.Content, "not found") {
		t.Fatalf("cross-parent wait = %s", denied.Content)
	}
}
func TestAgentLifecycleParentCancellationDoesNotKillChild(t *testing.T) {
	runner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	spawn.SetRunner(runner)
	parent, cancel := context.WithCancel(llm.ContextWithSessionID(context.Background(), "parent"))
	first := lifecycleResult(t, lifecycleCall(t, spawn, parent, `{"agent_name":"developer","prompt":"work","wait":0}`))
	<-runner.entered
	cancel()
	wait := &agentControlTool{name: WaitAgentToolName, spawn: spawn}
	snapshot := lifecycleCall(t, wait, llm.ContextWithSessionID(context.Background(), "parent"), `{"agent_ids":["`+first.AgentID+`"],"max_wait":0}`)
	if !strings.Contains(snapshot.Content, `"status":"running"`) {
		t.Fatalf("cancelled parent killed child: %s", snapshot.Content)
	}
	if err := spawn.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot = lifecycleCall(t, wait, llm.ContextWithSessionID(context.Background(), "parent"), `{"agent_ids":["`+first.AgentID+`"]}`)
	if !strings.Contains(snapshot.Content, `"status":"interrupted"`) {
		t.Fatalf("shutdown = %s", snapshot.Content)
	}
}

type resumedEventRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *resumedEventRunner) RunAgent(context.Context, string, string, int) (SpawnAgentRunResult, error) {
	return SpawnAgentRunResult{}, nil
}
func (r *resumedEventRunner) RunAgentWithCallback(context.Context, string, string, int, string, SubagentEventCallback) (SpawnAgentRunResult, error) {
	return SpawnAgentRunResult{}, nil
}
func (r *resumedEventRunner) SteerAgent(string, string) (string, string) { return "", "undelivered" }
func (r *resumedEventRunner) ContinueAgent(ctx context.Context, id, _, _ string, _ int, callID string, _ SpawnAgentRunOptions, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	if callID == "" {
		return SpawnAgentRunResult{}, fmt.Errorf("missing resume call ID")
	}
	close(r.started)
	select {
	case <-r.release:
	case <-ctx.Done():
		return SpawnAgentRunResult{}, ctx.Err()
	}
	cb(callID, SubagentEvent{Type: SubagentEventUsage, CountsTurn: true})
	cb(callID, SubagentEvent{Type: SubagentEventToolEnd, ToolName: "read_file"})
	return SpawnAgentRunResult{SessionID: id, Output: "resumed"}, nil
}

func TestResumeEventsCountTurnsAndReachAttachedWaiter(t *testing.T) {
	runner := &resumedEventRunner{started: make(chan struct{}), release: make(chan struct{})}
	owner := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	owner.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	record := session.AgentRun{ID: session.NewID(), ParentSessionID: "parent", AgentName: "developer", Prompt: "work", Started: true, Status: "completed", TurnsUsed: 1}
	e, err := owner.manager.start(ctx, record.AgentName, record.Prompt, "", "continue-call", nil, nil, runner, 1, true, "continue", record)
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	var received []SubagentEventType
	attached := owner.manager.attach(e, func(id string, event SubagentEvent) {
		if id != "wait-call" {
			t.Errorf("waiter call ID = %q", id)
		}
		received = append(received, event.Type)
	}, "wait-call")
	close(runner.release)
	<-e.done
	owner.manager.detach(e, attached)
	if e.record.TurnsUsed != 2 {
		t.Fatalf("resume turns = %d, want 2", e.record.TurnsUsed)
	}
	if len(received) != 2 || received[0] != SubagentEventUsage || received[1] != SubagentEventToolEnd {
		t.Fatalf("waiter events = %v", received)
	}
}

func TestReleaseCollectedDoesNotEvictReplacement(t *testing.T) {
	m := newAgentManager(SpawnConfig{MaxParallel: 1})
	// A persisted terminal entry is collected while a continuation with the
	// same ID is admitted. Collection must not delete the replacement.
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "runs.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m.store = store
	const id = "replacement-race"
	old := &agentEntry{record: session.AgentRun{ID: id, Status: "completed", CollectedAt: time.Now()}, done: make(chan struct{}), manager: m}
	close(old.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newEntry := &agentEntry{record: session.AgentRun{ID: id, Status: "running"}, done: make(chan struct{}), cancel: cancel, manager: m}
	go func() { <-ctx.Done(); close(newEntry.done) }()
	m.mu.Lock()
	m.agents[id] = old
	processAgentEntries.Store(id, old)
	finished := make(chan struct{})
	go func() { m.releaseCollected(old); close(finished) }()
	m.agents[id] = newEntry
	processAgentEntries.Store(id, newEntry)
	m.mu.Unlock()
	<-finished
	if m.agents[id] != newEntry {
		t.Fatal("collected old entry deleted active replacement")
	}
	if current, ok := processAgentEntries.Load(id); !ok || current != newEntry {
		t.Fatal("process-wide replacement evicted")
	}
	shutdown, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if err := m.Shutdown(shutdown); err != nil {
		t.Fatalf("replacement was not cancelled: %v", err)
	}
	processAgentEntries.CompareAndDelete(id, newEntry)
}

func TestCollectedRunEvictsAfterDoneCloses(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "runs.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	m := newAgentManager(SpawnConfig{MaxParallel: 1})
	m.store = store
	e, err := m.start(llm.ContextWithSessionID(context.Background(), "parent"), "developer", "work", "", "call", nil, nil, runner, 0, false, "", session.AgentRun{})
	if err != nil {
		t.Fatal(err)
	}
	<-runner.entered
	m.mu.Lock()
	e.record.CollectedAt = time.Now()
	m.mu.Unlock()
	m.releaseCollected(e) // the live run cannot yet be evicted
	close(runner.release)
	<-e.done
	deadline := time.After(time.Second)
	for {
		m.mu.Lock()
		_, exists := m.agents[e.record.ID]
		m.mu.Unlock()
		if !exists {
			return
		}
		select {
		case <-deadline:
			t.Fatal("terminal collected entry not evicted on completion")
		case <-time.After(time.Millisecond):
		}
	}
}
