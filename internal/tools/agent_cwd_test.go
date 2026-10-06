package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type cwdLifecycleRunner struct {
	*lifecycleRunner
	store session.AgentRunStore
	dir   string
	seen  chan SpawnAgentRunOptions
}

func (r *cwdLifecycleRunner) AgentWorkingDir() string              { return r.dir }
func (r *cwdLifecycleRunner) AgentRunStore() session.AgentRunStore { return r.store }
func (r *cwdLifecycleRunner) RunAgentWithCallbackAndOptions(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	r.seen <- opts
	return r.lifecycleRunner.RunAgentWithCallback(ctx, name, prompt, depth, id, cb)
}
func (r *cwdLifecycleRunner) ContinueAgent(ctx context.Context, id, name, instructions string, depth int, callID string, opts SpawnAgentRunOptions, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	r.seen <- opts
	return SpawnAgentRunResult{Output: "continued"}, nil
}
func (r *cwdLifecycleRunner) SteerAgent(string, string) (string, string) { return "", "undelivered" }

func TestSpawnAgentCWDAndContinuation(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := &cwdLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 8), release: make(chan struct{})}, store: store, dir: root, seen: make(chan SpawnAgentRunOptions, 3)}
	close(runner.release)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	for _, tc := range []struct{ cwd, want string }{{"child", child}, {child, child}, {"", root}} {
		tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
		tool.SetRunner(runner)
		args, _ := json.Marshal(SpawnAgentArgs{AgentName: "developer", Prompt: "work", CWD: tc.cwd, Wait: new(int)})
		result := lifecycleResult(t, lifecycleCall(t, tool, ctx, string(args)))
		<-runner.seen
		if err := tool.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetAgentRun(ctx, result.AgentID)
		if err != nil || stored.BaseDir != tc.want {
			t.Fatalf("cwd %q: %+v, %v", tc.cwd, stored, err)
		}
		if tc.cwd == "child" {
			tool2 := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
			runner2 := &cwdLifecycleRunner{lifecycleRunner: &lifecycleRunner{}, store: store, dir: root, seen: make(chan SpawnAgentRunOptions, 1)}
			tool2.SetRunner(runner2)
			lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: tool2}, ctx, `{"agent_id":"`+result.AgentID+`","wait":0}`)
			if opts := <-runner2.seen; opts.BaseDir != child {
				t.Fatalf("continuation base dir %q, want %q", opts.BaseDir, child)
			}
			if err := tool2.Drain(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestSpawnAgentSessionlessWaitIsNotExecutionDeadline(t *testing.T) {
	runner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(runner)
	result := make(chan llm.ToolOutput, 1)
	go func() {
		out, _ := tool.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"work","wait":0}`))
		result <- out
	}()
	<-runner.entered
	select {
	case out := <-result:
		t.Fatalf("sessionless child was stopped by wait=0: %s", out.Content)
	default:
	}
	close(runner.release)
	out := <-result
	if !strings.Contains(out.Content, `"status":"completed"`) || strings.Contains(out.Content, `"next"`) {
		t.Fatalf("sessionless completion = %s", out.Content)
	}
}

func TestSpawnAgentRejectsInvalidCWD(t *testing.T) {
	root := t.TempDir()
	runner := &cwdLifecycleRunner{lifecycleRunner: &lifecycleRunner{}, dir: root, seen: make(chan SpawnAgentRunOptions, 1)}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(runner)
	for _, cwd := range []string{"missing", filepath.Join(root, "missing")} {
		args, _ := json.Marshal(SpawnAgentArgs{AgentName: "developer", Prompt: "work", CWD: cwd})
		out := lifecycleCall(t, tool, llm.ContextWithSessionID(context.Background(), "parent"), string(args))
		if !out.IsError || !strings.Contains(out.Content, "cwd must be an existing directory") {
			t.Fatalf("cwd %q: %+v", cwd, out)
		}
	}
	runner.dir = ""
	out := lifecycleCall(t, tool, llm.ContextWithSessionID(context.Background(), "parent"), `{"agent_name":"developer","prompt":"work","cwd":"child"}`)
	if !out.IsError || !strings.Contains(out.Content, "relative cwd requires a bound parent") {
		t.Fatalf("unbound: %+v", out)
	}
}
