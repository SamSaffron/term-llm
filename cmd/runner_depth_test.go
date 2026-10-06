package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestCmdRunnerPrepareCapsSubagentSpawnBudget(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	for _, tc := range []struct {
		name              string
		own, budget, want int
	}{
		{"caller caps own allowance", 3, 1, 1},
		{"own allowance caps caller", 1, 2, 1},
		{"exhausted", 3, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentDir := t.TempDir()
			manifest := fmt.Sprintf("name: depth-probe\ndescription: Test depth propagation\ntools:\n  enabled: [spawn_agent]\nspawn:\n  max_depth: %d\n", tc.own)
			if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
			env, err := runner.prepare(context.Background(), runpkg.Request{
				Platform: runpkg.PlatformConsole, IsSubagent: true, Depth: 1, RemainingDepth: &tc.budget,
				AgentName: agentDir, ProviderInstance: llm.NewMockProvider("mock"), Cwd: t.TempDir(), DeferSession: true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			spawn := env.runtime.toolMgr.GetSpawnAgentTool()
			if spawn == nil || spawn.RemainingDepth() != tc.want {
				t.Fatalf("prepared spawn remaining = %v, want %d", spawn, tc.want)
			}
			if tc.want == 0 {
				out, err := spawn.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"work"}`))
				if err != nil || !strings.Contains(out.Content, "spawn depth budget exhausted") {
					t.Fatalf("exhausted spawn = %q, %v", out.Content, err)
				}
			}
		})
	}
}

func TestCmdDeveloperChainStopsAtThirdLevel(t *testing.T) {
	cfg := &config.Config{}
	runner := &SpawnAgentRunner{cfg: cfg}
	developer := &agents.Agent{Name: "developer", Tools: agents.ToolsConfig{Enabled: []string{tools.SpawnAgentToolName}}, Spawn: agents.SpawnConfig{MaxDepth: 1, AllowedAgents: []string{"developer"}}}
	makeTool := func(depth, budget int) *tools.SpawnAgentTool {
		t.Helper()
		mgr, err := runner.setupAgentToolsWithBudget(cfg, llm.NewEngine(llm.NewMockProvider("mock"), nil), developer, depth, "developer-session", budget)
		if err != nil {
			t.Fatal(err)
		}
		spawn := mgr.GetSpawnAgentTool()
		if spawn == nil {
			t.Fatal("developer spawn tool missing")
		}
		return spawn
	}
	first := makeTool(0, 1)
	capture := &capturingSpawnRunner{}
	first.SetRunner(capture)
	out, err := first.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"second"}`))
	if err != nil || capture.lastDepth != 1 || capture.lastOptions.RemainingDepth == nil || *capture.lastOptions.RemainingDepth != 0 {
		t.Fatalf("first developer delegation = %q, %v; depth=%d options=%+v", out.Content, err, capture.lastDepth, capture.lastOptions)
	}
	second := makeTool(capture.lastDepth, *capture.lastOptions.RemainingDepth)
	second.SetRunner(&capturingSpawnRunner{})
	out, err = second.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"third"}`))
	if err != nil || !strings.Contains(out.Content, "spawn depth budget exhausted") {
		t.Fatalf("third developer should be denied: %q, %v", out.Content, err)
	}
}
