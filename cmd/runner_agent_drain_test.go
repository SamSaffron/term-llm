package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

type blockingChildRunner struct {
	entered chan struct{}
	release chan struct{}
}

func (r *blockingChildRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithCallback(ctx, name, prompt, depth, "", nil)
}
func (r *blockingChildRunner) RunAgentWithCallback(ctx context.Context, name, prompt string, depth int, id string, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	close(r.entered)
	select {
	case <-r.release:
		return tools.SpawnAgentRunResult{Output: "done"}, nil
	case <-ctx.Done():
		return tools.SpawnAgentRunResult{}, ctx.Err()
	}
}

type uninterruptibleChildRunner struct {
	entered chan struct{}
	release chan struct{}
}

func (r *uninterruptibleChildRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithCallback(ctx, name, prompt, depth, "", nil)
}
func (r *uninterruptibleChildRunner) RunAgentWithCallback(_ context.Context, _, _ string, _ int, _ string, _ tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	close(r.entered)
	<-r.release // A host prompt may not accept context cancellation.
	return tools.SpawnAgentRunResult{}, nil
}

func TestSpawnRunnerShutdownHonorsDeadlineWithBlockedPrompt(t *testing.T) {
	child := &uninterruptibleChildRunner{entered: make(chan struct{}), release: make(chan struct{})}
	tool := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(child)
	runner := &SpawnAgentRunner{lifecycle: tool}
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	if _, err := tool.Execute(ctx, []byte(`{"agent_name":"developer","prompt":"work","wait":0}`)); err != nil {
		t.Fatal(err)
	}
	<-child.entered
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runner.Shutdown(deadline); err != context.DeadlineExceeded {
		t.Fatalf("blocked prompt shutdown = %v, want deadline exceeded", err)
	}
	close(child.release)
}

func TestRunEnvironmentDrainsDetachedChild(t *testing.T) {
	child := &blockingChildRunner{entered: make(chan struct{}), release: make(chan struct{})}
	tool := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(child)
	runner := &SpawnAgentRunner{lifecycle: tool}
	env := &cmdRunEnvironment{req: runpkg.Request{IsSubagent: true}, runtime: &serveRuntime{spawnRunner: runner}, runCtx: context.Background()}
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	result, err := tool.Execute(ctx, []byte(`{"agent_name":"developer","prompt":"work","wait":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Content == "" {
		t.Fatal("missing agent result")
	}
	<-child.entered
	closed := make(chan struct{})
	go func() { env.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("normal completion cancelled or abandoned detached child")
	case <-time.After(300 * time.Millisecond):
	}
	close(child.release)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not finish")
	}
}

func TestRunEnvironmentWithoutOwnerDrainsDetachedChild(t *testing.T) {
	child := &blockingChildRunner{entered: make(chan struct{}), release: make(chan struct{})}
	tool := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(child)
	runner := &SpawnAgentRunner{lifecycle: tool}
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	if _, err := tool.Execute(ctx, []byte(`{"agent_name":"developer","prompt":"work","wait":0}`)); err != nil {
		t.Fatal(err)
	}
	<-child.entered
	storeClosed := make(chan struct{})
	env := &cmdRunEnvironment{runtime: &serveRuntime{spawnRunner: runner}, runCtx: ctx, closeStore: func() { close(storeClosed) }}
	finished := make(chan struct{})
	go func() { env.Close(); close(finished) }()
	select {
	case <-finished:
		t.Fatal("owner-less run abandoned detached child")
	case <-storeClosed:
		t.Fatal("store closed before child finished")
	default:
	}
	close(child.release)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("owner-less run did not drain")
	}
	select {
	case <-storeClosed:
	default:
		t.Fatal("store not closed after drain")
	}
}

func TestAgentHostOwnerRejectsAdoptionAfterShutdown(t *testing.T) {
	owner := &agentHostOwner{}
	if err := owner.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := &blockingChildRunner{entered: make(chan struct{}), release: make(chan struct{})}
	tool := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(child)
	runner := &SpawnAgentRunner{lifecycle: tool}
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	if _, err := tool.Execute(ctx, []byte(`{"agent_name":"developer","prompt":"work","wait":0}`)); err != nil {
		t.Fatal(err)
	}
	<-child.entered
	closed := false
	owner.adopt(&serveRuntime{spawnRunner: runner}, func() { closed = true })
	if !closed || len(runner.OutstandingAgentIDs()) != 0 {
		t.Fatalf("late child survived shutdown: store closed=%t outstanding=%v", closed, runner.OutstandingAgentIDs())
	}
}

func TestRunEnvironmentHandsOffDetachedChildAcrossTurns(t *testing.T) {
	child := &blockingChildRunner{entered: make(chan struct{}), release: make(chan struct{})}
	tool := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(child)
	owner := &agentHostOwner{}
	runner := &SpawnAgentRunner{lifecycle: tool}
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	res, err := tool.Execute(ctx, []byte(`{"agent_name":"developer","prompt":"work","wait":0}`))
	if err != nil {
		t.Fatal(err)
	}
	<-child.entered
	closed := make(chan struct{})
	env := &cmdRunEnvironment{runtime: &serveRuntime{spawnRunner: runner}, agentOwner: owner, runCtx: ctx}
	go func() { env.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("parent turn blocked on child")
	}
	if len(runner.OutstandingAgentIDs()) != 1 {
		t.Fatalf("child lost between turns: %s", res.Content)
	}
	close(child.release)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := owner.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestSubagentDrainCancellationShutsDownGrandchild(t *testing.T) {
	child := &blockingChildRunner{entered: make(chan struct{}), release: make(chan struct{})}
	tool := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(child)
	runner := &SpawnAgentRunner{lifecycle: tool}
	ctx, cancel := context.WithCancel(llm.ContextWithSessionID(context.Background(), "child"))
	defer cancel()
	if _, err := tool.Execute(ctx, []byte(`{"agent_name":"developer","prompt":"grandchild","wait":0}`)); err != nil {
		t.Fatal(err)
	}
	<-child.entered
	env := &cmdRunEnvironment{req: runpkg.Request{IsSubagent: true}, runtime: &serveRuntime{spawnRunner: runner}, runCtx: ctx}
	done := make(chan struct{})
	go func() { env.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("drain returned before cancellation")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("grandchild was not stopped on cancellation")
	}
	if len(runner.OutstandingAgentIDs()) != 0 {
		t.Fatal("grandchild is still running")
	}
}
