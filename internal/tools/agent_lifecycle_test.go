package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

type lifecycleRunner struct {
	entered chan string
	release chan struct{}
	mu      sync.Mutex
	calls   int
	resumed int
	steered string
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
func (r *lifecycleRunner) ContinueAgent(_ context.Context, id, name, instructions string, _ int, _ SpawnAgentRunOptions, _ SubagentEventCallback) (SpawnAgentRunResult, error) {
	r.mu.Lock()
	r.resumed++
	r.mu.Unlock()
	return SpawnAgentRunResult{Output: "resumed", SessionID: id}, nil
}
func (r *lifecycleRunner) SteerAgent(_ string, instructions string) (string, string) {
	r.mu.Lock()
	r.steered = instructions
	r.mu.Unlock()
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
func TestAgentLifecycleCrossManagerWait(t *testing.T) {
	runner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	first := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	first.SetRunner(runner)
	second := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	second.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "shared-parent")
	spawned := lifecycleResult(t, lifecycleCall(t, first, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	<-runner.entered
	wait := &agentControlTool{name: WaitAgentToolName, spawn: second}
	result := lifecycleCall(t, wait, ctx, `{"agent_ids":["`+spawned.AgentID+`"],"max_wait":0}`)
	if !strings.Contains(result.Content, `"status":"running"`) {
		t.Fatalf("cross-manager wait = %s", result.Content)
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
	if !strings.Contains(output.Content, `"intervention_disposition":"queued"`) {
		t.Fatalf("steering = %s", output.Content)
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
