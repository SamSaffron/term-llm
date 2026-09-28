package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

type cmdApprovalSink struct {
	entered chan struct{}
	release chan struct{}
}

func (*cmdApprovalSink) Event(llm.Event) {}
func (s *cmdApprovalSink) PromptApproval(string, bool, bool, string) (tools.ApprovalResult, error) {
	close(s.entered)
	<-s.release
	return tools.ApprovalResult{}, nil
}

type cmdApprovalChild struct{ root *tools.ApprovalManager }

func (r *cmdApprovalChild) AgentApprovalScope(parent string) *tools.ApprovalManager {
	return r.root.CloneForAgentRun(parent)
}
func (r *cmdApprovalChild) RunAgent(ctx context.Context, name, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithCallback(ctx, name, prompt, depth, "", nil)
}
func (r *cmdApprovalChild) RunAgentWithCallback(ctx context.Context, _, _ string, _ int, _ string, _ tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	scope := tools.AgentApprovalScopeFromContext(ctx)
	if scope == nil || scope.PromptUIFunc == nil {
		return tools.SpawnAgentRunResult{}, context.Canceled
	}
	_, err := scope.PromptUIFunc("review.txt", false, false, "")
	return tools.SpawnAgentRunResult{Output: "approved"}, err
}

func TestCmdRunnerHostPromptTracksDetachedApproval(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	sink := &cmdApprovalSink{entered: make(chan struct{}), release: make(chan struct{})}
	runner := newCmdRunner(cfg, cmdRunnerOptions{Tools: "spawn_agent", ToolsSet: true, ApprovalMode: tools.ModePrompt, ApprovalModeSet: true}).(*cmdRunner)
	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform: runpkg.PlatformConsole, SessionID: "parent", ProviderInstance: llm.NewMockProvider("mock"), Cwd: t.TempDir(), DeferSession: true,
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	mgr := env.runtime.toolMgr
	if mgr == nil || mgr.ApprovalMgr.PromptUIFunc == nil {
		t.Fatal("host prompt was not wired")
	}
	spawn := mgr.GetSpawnAgentTool()
	if spawn == nil {
		t.Fatal("spawn tool was not wired")
	}
	spawn.SetRunner(&cmdApprovalChild{root: mgr.ApprovalMgr})
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	out, err := spawn.Execute(ctx, json.RawMessage(`{"agent_name":"developer","prompt":"needs approval","wait":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var spawned tools.SpawnAgentResult
	if err := json.Unmarshal([]byte(out.Content), &spawned); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("host prompt never ran")
	}
	list, ok := mgr.Registry.Get(tools.ListAgentsToolName)
	if !ok {
		t.Fatal("list_agents missing")
	}
	status, err := list.Execute(ctx, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(status.Content, `"status":"awaiting_approval"`) || !strings.Contains(status.Content, spawned.AgentID) {
		t.Fatalf("host prompt status = %s, %v", status.Content, err)
	}
	close(sink.release)
	if err := spawn.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = list.Execute(ctx, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(status.Content, `"status":"completed"`) {
		t.Fatalf("approved child status = %s, %v", status.Content, err)
	}
}
