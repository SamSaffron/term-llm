package cmd

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
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
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	manager.SetupEngine(engine)
	capability := runtimeAgentMentionCapability{engine: engine, manager: manager}

	names, err := capability.PermittedAgentNames()
	if err != nil || len(names) != 2 || names[0] != "codebase" || names[1] != "reviewer" {
		t.Fatalf("runtime intersection = %#v, err=%v", names, err)
	}

	engine.SetAllowedToolsFilter([]string{})
	if _, err := capability.PermittedAgentNames(); err == nil || !strings.Contains(err.Error(), "active tool restriction") {
		t.Fatalf("filtered catalog error = %v", err)
	}
	if err := capability.ValidateAgentMention("codebase"); err == nil || !strings.Contains(err.Error(), "active tool restriction") {
		t.Fatalf("filtered exact error = %v", err)
	}

	engine.RestoreAllowedToolsFilter(nil, false)
	if err := capability.ValidateAgentMention("codebase"); err != nil {
		t.Fatalf("restored filter validation = %v", err)
	}
	manager.GetSpawnAgentTool().SetDepth(2)
	if err := capability.ValidateAgentMention("codebase"); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth validation = %v", err)
	}
}

func TestRuntimeAgentMentionCapabilityFailsClosedWithoutActualSpawnTool(t *testing.T) {
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	capability := runtimeAgentMentionCapability{engine: engine, manager: &tools.ToolManager{}}
	if _, err := capability.PermittedAgentNames(); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("missing tool error = %v", err)
	}
}

func TestRuntimeAgentMentionCapabilityUsesBuiltinAgentSpawnPolicy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Agents.UseBuiltin = true
	runner, err := NewSpawnAgentRunner(cfg, false, nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		active string
		want   []string
	}{
		{active: "developer", want: []string{"codebase", "reviewer", "web-researcher"}},
		{active: "active-review", want: []string{"developer", "reviewer"}},
	}
	for _, tt := range tests {
		t.Run(tt.active, func(t *testing.T) {
			active, err := runner.registry.Get(tt.active)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := tools.NewToolManager(&tools.ToolConfig{
				Enabled: []string{tools.SpawnAgentToolName},
				Spawn: tools.SpawnConfig{
					MaxParallel:    active.Spawn.MaxParallel,
					MaxDepth:       active.Spawn.MaxDepth,
					DefaultTimeout: active.Spawn.DefaultTimeout,
					AllowedAgents:  active.Spawn.AllowedAgents,
					AgentModels:    active.Spawn.AgentModels,
				},
			}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			manager.GetSpawnAgentTool().SetRunner(runner)
			engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
			manager.SetupEngine(engine)
			capability := runtimeAgentMentionCapability{engine: engine, manager: manager}

			got, err := capability.PermittedAgentNames()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("permitted agents = %#v, want %#v", got, tt.want)
			}
		})
	}
}
