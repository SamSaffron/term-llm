package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestRuntimeAgentMentionCapabilityTracksRegisteredToolAndLiveEngineFilter(t *testing.T) {
	manager, err := tools.NewToolManager(&tools.ToolConfig{
		Enabled: []string{tools.SpawnAgentToolName},
		Spawn: tools.SpawnConfig{
			MaxDepth:       2,
			AllowedAgents:  []string{"codebase", "reviewer"},
			MaxParallel:    1,
			DefaultTimeout: 30,
		},
	}, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runner := &capturingSpawnRunner{catalogNames: []string{"codebase", "reviewer", "web-researcher"}}
	manager.GetSpawnAgentTool().SetRunner(runner)
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	manager.SetupEngine(eng)
	currentEngine := eng
	capability := RuntimeAgentMentionCapability{Engine: func() *engine.Engine { return currentEngine }, Manager: manager}

	names, err := capability.PermittedAgentNames()
	if err != nil || len(names) != 2 || names[0] != "codebase" || names[1] != "reviewer" {
		t.Fatalf("runtime intersection = %#v, err=%v", names, err)
	}

	eng.SetAllowedToolsFilter([]string{})
	if _, err := capability.PermittedAgentNames(); err == nil || !strings.Contains(err.Error(), "active tool restriction") {
		t.Fatalf("filtered catalog error = %v", err)
	}
	if err := capability.ValidateAgentMention("codebase"); err == nil || !strings.Contains(err.Error(), "active tool restriction") {
		t.Fatalf("filtered exact error = %v", err)
	}

	eng.RestoreAllowedToolsFilter(nil, false)
	if err := capability.ValidateAgentMention("codebase"); err != nil {
		t.Fatalf("restored filter validation = %v", err)
	}

	deniedEngine := engine.NewEngine(mockprovider.NewMockProvider("denied"), nil)
	manager.SetupEngine(deniedEngine)
	deniedEngine.SetAllowedToolsFilter([]string{})
	currentEngine = deniedEngine
	if err := capability.ValidateAgentMention("codebase"); err == nil || !strings.Contains(err.Error(), "active tool restriction") {
		t.Fatalf("allow-to-deny replacement used stale eng: %v", err)
	}

	allowedEngine := engine.NewEngine(mockprovider.NewMockProvider("allowed"), nil)
	manager.SetupEngine(allowedEngine)
	currentEngine = allowedEngine
	if err := capability.ValidateAgentMention("codebase"); err != nil {
		t.Fatalf("deny-to-allow replacement used stale eng: %v", err)
	}

	manager.GetSpawnAgentTool().SetRemainingDepth(0)
	if err := capability.ValidateAgentMention("codebase"); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth validation = %v", err)
	}
}

func TestRuntimeAgentMentionCapabilityFailsClosedWithoutActualSpawnTool(t *testing.T) {
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	capability := RuntimeAgentMentionCapability{Engine: func() *engine.Engine { return eng }, Manager: &tools.ToolManager{}}
	if _, err := capability.PermittedAgentNames(); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("missing tool error = %v", err)
	}
}

type capturingSpawnRunner struct {
	catalogNames []string
}

func (r *capturingSpawnRunner) ListSpawnAgentNames() ([]string, error) {
	return append([]string(nil), r.catalogNames...), nil
}

func (r *capturingSpawnRunner) ResolveSpawnAgent(name string) error {
	for _, candidate := range r.catalogNames {
		if candidate == name {
			return nil
		}
	}
	return errors.New("definition not found")
}

func (r *capturingSpawnRunner) RunAgent(ctx context.Context, agentName string, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return tools.SpawnAgentRunResult{Output: "ok"}, nil
}

func (r *capturingSpawnRunner) RunAgentWithCallback(ctx context.Context, agentName string, prompt string, depth int, callID string, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	return tools.SpawnAgentRunResult{Output: "ok"}, nil
}
