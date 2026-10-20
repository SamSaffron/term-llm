package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestRuntimeAgentMentionCapabilityUsesBuiltinAgentSpawnPolicy(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Agents.UseBuiltin = true
	runner, err := appserve.NewSpawnAgentRunner(cfg, false, nil)
	if err != nil {
		t.Fatal(err)
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{UseBuiltin: cfg.Agents.UseBuiltin, SearchPaths: cfg.Agents.SearchPaths})
	if err != nil {
		t.Fatal(err)
	}
	registry.SetPreferences(cfg.Agents.Preferences)

	tests := []struct {
		active string
		want   []string
	}{
		{active: "developer", want: []string{"codebase", "developer", "reviewer", "web-researcher"}},
		{active: "active-review", want: []string{"developer", "reviewer"}},
	}
	for _, tt := range tests {
		t.Run(tt.active, func(t *testing.T) {
			active, err := registry.Get(tt.active)
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
			eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
			manager.SetupEngine(eng)
			capability := RuntimeAgentMentionCapability{Engine: func() *engine.Engine { return eng }, Manager: manager}

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
