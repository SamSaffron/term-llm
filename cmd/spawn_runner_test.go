package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

type capturingSpawnRunner struct {
	lastAgentName string
	lastPrompt    string
	lastDepth     int
	lastOptions   tools.SpawnAgentRunOptions
}

func (r *capturingSpawnRunner) RunAgent(ctx context.Context, agentName string, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithOptions(ctx, agentName, prompt, depth, tools.SpawnAgentRunOptions{})
}

func (r *capturingSpawnRunner) RunAgentWithCallback(ctx context.Context, agentName string, prompt string, depth int, callID string, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithCallbackAndOptions(ctx, agentName, prompt, depth, callID, cb, tools.SpawnAgentRunOptions{})
}

func (r *capturingSpawnRunner) RunAgentWithOptions(ctx context.Context, agentName string, prompt string, depth int, opts tools.SpawnAgentRunOptions) (tools.SpawnAgentRunResult, error) {
	r.lastAgentName = agentName
	r.lastPrompt = prompt
	r.lastDepth = depth
	r.lastOptions = opts
	return tools.SpawnAgentRunResult{Output: "ok"}, nil
}

func (r *capturingSpawnRunner) RunAgentWithCallbackAndOptions(ctx context.Context, agentName string, prompt string, depth int, callID string, cb tools.SubagentEventCallback, opts tools.SpawnAgentRunOptions) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithOptions(ctx, agentName, prompt, depth, opts)
}

func TestSpawnRunnerBuildRunRequestInheritsParentBaseDir(t *testing.T) {
	runner := &SpawnAgentRunner{}
	runner.SetBaseDir("  /tmp/parent-worktree  ")

	req := runner.buildRunRequest(context.Background(), "reviewer", "review this", "child-session", 1, false, tools.SpawnAgentRunOptions{})
	if req.Cwd != "/tmp/parent-worktree" {
		t.Fatalf("Cwd = %q, want inherited parent BaseDir", req.Cwd)
	}
}

func TestSpawnRunnerBuildRunRequestUsesCurrentParentContext(t *testing.T) {
	baseDir := "/tmp/first-worktree"
	runner := &SpawnAgentRunner{parentSessionID: "stale-parent"}
	runner.SetBaseDirFunc(func() string { return baseDir })

	baseDir = "/tmp/current-worktree"
	ctx := llm.ContextWithSessionID(context.Background(), "current-parent")
	req := runner.buildRunRequest(ctx, "reviewer", "review this", "child-session", 1, false, tools.SpawnAgentRunOptions{})
	if req.Cwd != "/tmp/current-worktree" {
		t.Fatalf("Cwd = %q, want current parent BaseDir", req.Cwd)
	}
	if req.ParentSessionID != "current-parent" {
		t.Fatalf("ParentSessionID = %q, want current context session", req.ParentSessionID)
	}
}

func TestSpawnRunnerBuildRunRequestFallsBackToConfiguredContext(t *testing.T) {
	runner := &SpawnAgentRunner{parentSessionID: "configured-parent"}
	runner.SetBaseDir("/tmp/configured-worktree")
	runner.SetBaseDirFunc(func() string { return "" })

	req := runner.buildRunRequest(context.Background(), "reviewer", "review this", "child-session", 1, false, tools.SpawnAgentRunOptions{})
	if req.Cwd != "/tmp/configured-worktree" {
		t.Fatalf("Cwd = %q, want configured BaseDir fallback", req.Cwd)
	}
	if req.ParentSessionID != "configured-parent" {
		t.Fatalf("ParentSessionID = %q, want configured parent fallback", req.ParentSessionID)
	}
}

func TestWireSpawnAgentRunnerTracksToolManagerBaseDir(t *testing.T) {
	first := t.TempDir()
	current := t.TempDir()
	cfg := &config.Config{}
	toolMgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{tools.SpawnAgentToolName}}, cfg)
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}
	if err := toolMgr.SetBaseDir(first); err != nil {
		t.Fatalf("SetBaseDir first: %v", err)
	}
	runner, err := WireSpawnAgentRunnerWithStore(cfg, toolMgr, false, nil, "parent-session")
	if err != nil {
		t.Fatalf("WireSpawnAgentRunnerWithStore: %v", err)
	}
	if err := toolMgr.SetBaseDir(current); err != nil {
		t.Fatalf("SetBaseDir current: %v", err)
	}

	req := runner.buildRunRequest(context.Background(), "reviewer", "review this", "child-session", 1, false, tools.SpawnAgentRunOptions{})
	if req.Cwd != current {
		t.Fatalf("Cwd = %q, want current tool manager BaseDir %q", req.Cwd, current)
	}
}

func TestSpawnRunnerSetupAgentToolsUsesCurrentBaseDir(t *testing.T) {
	first := t.TempDir()
	current := t.TempDir()
	runner := &SpawnAgentRunner{cfg: &config.Config{}}
	runner.SetBaseDir(first)
	runner.SetBaseDirFunc(func() string { return current })
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	agent := &agents.Agent{
		Name:  "parent",
		Tools: agents.ToolsConfig{Enabled: []string{tools.SpawnAgentToolName}},
	}

	toolMgr, err := runner.setupAgentTools(runner.cfg, engine, agent, 0, "child-session")
	if err != nil {
		t.Fatalf("setupAgentTools() error = %v", err)
	}
	if got := toolMgr.BaseDir(); got != current {
		t.Fatalf("BaseDir = %q, want current BaseDir %q", got, current)
	}
}

func TestSpawnRunnerSetupAgentToolsPropagatesAgentModels(t *testing.T) {
	cfg := &config.Config{}
	runner := &SpawnAgentRunner{cfg: cfg}
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	agent := &agents.Agent{
		Name: "parent",
		Tools: agents.ToolsConfig{
			Enabled: []string{tools.SpawnAgentToolName},
		},
		Spawn: agents.SpawnConfig{
			AgentModels: map[string]string{
				"codebase": "fast",
			},
		},
	}

	toolMgr, err := runner.setupAgentTools(cfg, engine, agent, 0, "child-session")
	if err != nil {
		t.Fatalf("setupAgentTools() error = %v", err)
	}
	spawnTool := toolMgr.GetSpawnAgentTool()
	if spawnTool == nil {
		t.Fatal("spawn_agent tool was not enabled")
	}

	capturingRunner := &capturingSpawnRunner{}
	spawnTool.SetRunner(capturingRunner)
	out, err := spawnTool.Execute(context.Background(), json.RawMessage(`{"agent_name":"codebase","prompt":"inspect"}`))
	if err != nil {
		t.Fatalf("spawn_agent Execute() error = %v", err)
	}
	if capturingRunner.lastOptions.ModelOverride != "fast" {
		t.Fatalf("ModelOverride = %q, want fast (output %q)", capturingRunner.lastOptions.ModelOverride, out.Content)
	}
}
