package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
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

func TestRunEnvironmentDrainsDetachedChild(t *testing.T) {
	child := &blockingChildRunner{entered: make(chan struct{}), release: make(chan struct{})}
	tool := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(child)
	runner := &SpawnAgentRunner{lifecycle: tool}
	env := &cmdRunEnvironment{runtime: &serveRuntime{spawnRunner: runner}, runCtx: context.Background()}
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
