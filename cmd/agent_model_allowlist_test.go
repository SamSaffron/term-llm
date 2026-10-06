package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestCheckAgentModelResolvesFastAlias(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "primary", Providers: map[string]config.ProviderConfig{
		"primary": {Model: "main", FastProvider: "small", FastModel: "quick"},
		"small":   {Model: "quick"},
	}}
	agent := &agents.Agent{Name: "reviewer", AllowedModels: []string{"small:quick"}}
	if err := checkAgentModel(agent, cfg, true); err != nil {
		t.Fatalf("fast model should be allowed: %v", err)
	}
	if err := checkAgentModel(agent, cfg, false); err == nil {
		t.Fatal("main model should be denied")
	}
}

func TestCmdRunnerRejectsDisallowedAgentModelBeforeProviderCreation(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "reviewer")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: reviewer\nallowed_models: [debug:allowed]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "allowed"}}}
	for _, platform := range []string{runpkg.PlatformConsole, runpkg.PlatformWeb, runpkg.PlatformJob} {
		t.Run(platform, func(t *testing.T) {
			runner := &cmdRunner{baseCfg: cfg}
			_, err := runner.prepare(context.Background(), runpkg.Request{AgentName: agentDir, Model: "denied", Prompt: "hello", Platform: platform}, nil)
			if err == nil || !strings.Contains(err.Error(), "not allowed for agent") {
				t.Fatalf("prepare() error = %v, want model allowlist denial", err)
			}
		})
	}
}
