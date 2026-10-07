package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type persistentLifecycleRunner struct {
	*lifecycleRunner
	store session.AgentRunStore
}

func (r *persistentLifecycleRunner) AgentRunStore() session.AgentRunStore { return r.store }

func TestAgentCollectAcrossTurnStoreHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	firstStore, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	child := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	firstTool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	firstTool.SetRunner(&persistentLifecycleRunner{lifecycleRunner: child, store: firstStore})
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	first := lifecycleResult(t, lifecycleCall(t, firstTool, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	<-child.entered
	close(child.release)
	if err := firstTool.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}
	secondStore, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	secondTool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	secondTool.SetRunner(&persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{}, store: secondStore})
	out := lifecycleCall(t, &agentControlTool{name: WaitAgentToolName, spawn: secondTool}, ctx, `{"agent_ids":["`+first.AgentID+`"],"max_wait":0}`)
	if !strings.Contains(out.Content, `"status":"completed"`) {
		t.Fatalf("second-turn wait = %s", out.Content)
	}
	got, err := secondStore.GetAgentRun(ctx, first.AgentID)
	if err != nil || got.CollectedAt.IsZero() {
		t.Fatalf("second-turn collection = %+v, %v", got, err)
	}
}

// budgetRecordingRunner observes the budget passed to a queued relaunch or resume.
type budgetRecordingRunner struct {
	*persistentLifecycleRunner
	budget chan int
}

func (r *budgetRecordingRunner) RunAgentWithCallbackAndOptions(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	r.budget <- *opts.RemainingDepth
	return r.lifecycleRunner.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, id, cb, opts)
}

func (r *budgetRecordingRunner) ContinueAgent(ctx context.Context, id, name, instructions string, depth int, callID string, opts SpawnAgentRunOptions, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	r.budget <- *opts.RemainingDepth
	return r.lifecycleRunner.ContinueAgent(ctx, id, name, instructions, depth, callID, opts, cb)
}

func TestAgentLifecycleQueuedShutdownRestartsFreshAfterReload(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := &persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, store: store}
	firstTool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	firstTool.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	first := lifecycleResult(t, lifecycleCall(t, firstTool, ctx, `{"agent_name":"developer","prompt":"blocking","wait":0}`))
	<-runner.entered
	wait := &agentControlTool{name: WaitAgentToolName, spawn: firstTool}
	lifecycleCall(t, wait, ctx, `{"agent_ids":["`+first.AgentID+`"],"max_wait":0}`)
	before, err := store.GetAgentRun(ctx, first.AgentID)
	if err != nil || !before.CollectedAt.IsZero() {
		t.Fatalf("running agent collected: %+v, %v", before, err)
	}
	queued := lifecycleResult(t, lifecycleCall(t, firstTool, ctx, `{"agent_name":"developer","prompt":"queued","model":"test:model","wait":0}`))
	if queued.Status != "queued" {
		t.Fatalf("queued = %+v", queued)
	}
	if err := firstTool.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	lifecycleCall(t, wait, ctx, `{"agent_ids":["`+first.AgentID+`"],"max_wait":0}`)
	after, err := store.GetAgentRun(ctx, first.AgentID)
	if err != nil || after.CollectedAt.IsZero() {
		t.Fatalf("terminal agent not collected: %+v, %v", after, err)
	}
	if _, exists := processAgentEntries.Load(first.AgentID); exists {
		t.Fatal("collected terminal agent retained process-wide")
	}
	stored, err := store.GetAgentRun(ctx, queued.AgentID)
	if err != nil || stored.Status != "interrupted" || stored.Started || stored.Model != "test:model" {
		t.Fatalf("queued admission = %+v, %v", stored, err)
	}
	// Simulate a process restart: the new manager only has persisted admission.
	processAgentEntries.Delete(first.AgentID)
	processAgentEntries.Delete(queued.AgentID)
	fresh := &persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, store: store}
	replacement := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	replacement.SetRemainingDepth(1) // The resuming parent now has only one level.
	budgetRunner := &budgetRecordingRunner{persistentLifecycleRunner: fresh, budget: make(chan int, 1)}
	replacement.SetRunner(budgetRunner)
	continued := lifecycleResult(t, lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: replacement}, ctx, `{"agent_id":"`+queued.AgentID+`","wait":0}`))
	if continued.AgentID != queued.AgentID || continued.Status == "failed" {
		t.Fatalf("queued continuation = %+v", continued)
	}
	select {
	case id := <-fresh.entered:
		if id != queued.AgentID {
			t.Fatalf("new child id = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("queued child never started")
	}
	if got := <-budgetRunner.budget; got != 0 {
		t.Fatalf("queued child budget = %d, want 0 from resuming parent", got)
	}
	close(fresh.release)
	if err := replacement.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	if fresh.calls != 1 || fresh.resumed != 0 {
		t.Fatalf("queued child resumed without a session: calls=%d resumed=%d", fresh.calls, fresh.resumed)
	}
}

func TestAgentLifecycleReloadListInterruptedAndResume(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := &persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, store: store}
	firstTool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	firstTool.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	first := lifecycleResult(t, lifecycleCall(t, firstTool, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	<-runner.entered
	wait := &agentControlTool{name: WaitAgentToolName, spawn: firstTool}
	lifecycleCall(t, wait, ctx, `{"agent_ids":["`+first.AgentID+`"],"max_wait":0}`)
	before, err := store.GetAgentRun(ctx, first.AgentID)
	if err != nil || !before.CollectedAt.IsZero() {
		t.Fatalf("running agent collected: %+v, %v", before, err)
	}
	if err := firstTool.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The resuming parent's allowlist no longer names developer; an existing
	// child is still resumable while the parent's depth budget permits it.
	replacement := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300, AllowedAgents: []string{"codebase"}}, 0)
	replacement.SetRemainingDepth(1)
	budgetRunner := &budgetRecordingRunner{persistentLifecycleRunner: &persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, store: store}, budget: make(chan int, 1)}
	replacement.SetRunner(budgetRunner)
	listed := lifecycleCall(t, &agentControlTool{name: ListAgentsToolName, spawn: replacement}, ctx, `{}`)
	if !strings.Contains(listed.Content, `"status":"interrupted"`) || !strings.Contains(listed.Content, first.AgentID) {
		t.Fatalf("reloaded agents = %s", listed.Content)
	}
	// Strict-schema providers force the optional filter; "all" must not filter.
	listedAll := lifecycleCall(t, &agentControlTool{name: ListAgentsToolName, spawn: replacement}, ctx, `{"status":"all"}`)
	if !strings.Contains(listedAll.Content, first.AgentID) {
		t.Fatalf("status=all agents = %s", listedAll.Content)
	}
	continued := lifecycleResult(t, lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: replacement}, ctx, `{"agent_id":"`+first.AgentID+`","wait":1}`))
	if continued.Status != "completed" || continued.AgentID != first.AgentID {
		t.Fatalf("resumed record = %+v", continued)
	}
	if got := <-budgetRunner.budget; got != 0 {
		t.Fatalf("resumed child budget = %d, want 0 from resuming parent", got)
	}
	replacement.SetRemainingDepth(0)
	denied := lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: replacement}, ctx, `{"agent_id":"`+first.AgentID+`","wait":0}`)
	if !strings.Contains(denied.Content, "spawn depth budget exhausted") {
		t.Fatalf("exhausted parent resumed child: %s", denied.Content)
	}
}

func TestCompletedAgentNextIsFinal(t *testing.T) {
	out := agentOutput(session.AgentRun{ID: "child", Status: "completed", Output: "answer"})
	var result SpawnAgentResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Resumable || result.Output != "answer" || !strings.Contains(result.Next, "result is final") || strings.Contains(result.Next, `"continue"`) {
		t.Fatalf("completed result = %+v", result)
	}
}

type admissionRetryRunner struct {
	*persistentLifecycleRunner
	attempts int
}

func (r *admissionRetryRunner) ContinueAgent(ctx context.Context, id, name, instructions string, depth int, callID string, opts SpawnAgentRunOptions, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	r.attempts++
	if r.attempts == 1 {
		return SpawnAgentRunResult{}, &AgentRunAdmissionError{Err: errors.New("session is busy processing another request")}
	}
	return r.persistentLifecycleRunner.ContinueAgent(ctx, id, name, instructions, depth, callID, opts, cb)
}

func TestContinueAgentAdmissionFailurePreservesResumableRecord(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	child := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	runner := &admissionRetryRunner{persistentLifecycleRunner: &persistentLifecycleRunner{lifecycleRunner: child, store: store}}
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 5}, 0)
	spawn.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	initial := lifecycleResult(t, lifecycleCall(t, spawn, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	<-child.entered
	close(child.release)
	wait := &agentControlTool{name: WaitAgentToolName, spawn: spawn}
	lifecycleCall(t, wait, ctx, `{"agent_ids":["`+initial.AgentID+`"],"max_wait":1}`)
	before, err := store.GetAgentRun(ctx, initial.AgentID)
	if err != nil || before.Status != "completed" || before.Output != "done" {
		t.Fatalf("completed agent = %+v, %v", before, err)
	}
	control := &agentControlTool{name: ContinueAgentToolName, spawn: spawn}
	failed := lifecycleResult(t, lifecycleCall(t, control, ctx, `{"agent_id":"`+initial.AgentID+`","instructions":"average words","wait":5}`))
	if failed.Status != "completed" || !failed.Resumable || !strings.Contains(failed.Error, "session is busy") || !strings.Contains(failed.Next, "retry continue_agent") || !strings.Contains(failed.Next, "average words") {
		t.Fatalf("admission failure = %+v", failed)
	}
	after, err := store.GetAgentRun(ctx, initial.AgentID)
	if err != nil || after.Status != before.Status || after.StopReason != before.StopReason || after.Output != before.Output || after.Error != before.Error || after.TurnsUsed != before.TurnsUsed || after.TurnsGranted != before.TurnsGranted {
		t.Fatalf("admission failure changed durable record: before=%+v after=%+v err=%v", before, after, err)
	}
	retried := lifecycleResult(t, lifecycleCall(t, control, ctx, `{"agent_id":"`+initial.AgentID+`","instructions":"average words","wait":5}`))
	if retried.Status != "completed" || retried.Output != "resumed" || retried.AgentID != initial.AgentID || runner.attempts != 2 {
		t.Fatalf("retry = %+v; attempts=%d", retried, runner.attempts)
	}
}
