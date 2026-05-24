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
