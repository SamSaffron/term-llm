package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
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

func modelAllowlistFixture(t *testing.T, allowed string) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	agentDir := filepath.Join(root, "reviewer")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	definition := "name: reviewer\nprovider: debug\nmodel: allowed\nallowed_models: [" + allowed + "]\n"
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(definition), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{
		"debug": {Model: "allowed"}, "other": {Model: "denied"},
	}, Agents: config.AgentsConfig{SearchPaths: []string{root}}}
	return cfg, agentDir
}

func TestAgentModelPolicyRejectsCLIOverrides(t *testing.T) {
	for _, selection := range []string{"debug:denied", "other:denied"} {
		t.Run(selection, func(t *testing.T) {
			cfg, agentDir := modelAllowlistFixture(t, "debug:allowed")
			agent, err := LoadAgent(agentDir, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyAgentProviderModelPolicy(cfg, "", "", selection, agent, false); err == nil || !strings.Contains(err.Error(), "allowed: debug:allowed") {
				t.Fatalf("CLI override error = %v, want allowlist denial", err)
			}
		})
	}
}

func TestSpawnAgentRejectsOverridesOutsideProviderWildcard(t *testing.T) {
	for _, source := range []string{"tool argument", "parent configuration"} {
		t.Run(source, func(t *testing.T) {
			cfg, _ := modelAllowlistFixture(t, "debug:*")
			runner, err := NewSpawnAgentRunner(cfg, false, tools.NewApprovalManager(nil))
			if err != nil {
				t.Fatal(err)
			}
			defer runner.Wait()
			spawnCfg := tools.DefaultSpawnConfig()
			args := tools.SpawnAgentArgs{AgentName: "reviewer", Prompt: "hello"}
			if source == "tool argument" {
				args.Model = "other:denied"
			} else {
				spawnCfg.AgentModels = map[string]string{"reviewer": "other:denied"}
			}
			tool := tools.NewSpawnAgentTool(spawnCfg, 0)
			tool.SetRunner(runner)
			payload, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := tool.Execute(ctx, payload)
			if err != nil || !out.IsError || !strings.Contains(out.Content, "allowed: debug:*") {
				t.Fatalf("spawn output = %+v, error = %v, want allowlist denial", out, err)
			}
		})
	}
}

func TestJobsExecutorRejectsOverrideOutsideProviderWildcard(t *testing.T) {
	cfg, agentDir := modelAllowlistFixture(t, "debug:*")
	execute := newServeJobsExecutor(cfg, resolvedApprovalMode{Mode: tools.ModePrompt})
	_, err := execute(context.Background(), jobsV2LLMConfig{AgentName: agentDir, Model: "other:denied", Instructions: "hello"}, nil)
	if err == nil || !strings.Contains(err.Error(), "allowed: debug:*") {
		t.Fatalf("job execution error = %v, want allowlist denial", err)
	}
}

func TestWebModelSwapRejectsDisallowedModelAndKeepsRuntime(t *testing.T) {
	cfg, agentDir := modelAllowlistFixture(t, "debug:allowed")
	previous, provider := newCloseTrackingServeRuntime()
	previous.providerKey, previous.defaultModel, previous.agentName = "debug", "allowed", agentDir
	previous.history = []llm.Message{llm.UserText("hello")}
	manager := newServeSessionManager(time.Minute, 10, nil)
	t.Cleanup(manager.Close)
	putTestSession(manager, "allowlist-swap", previous)
	srv := &serveServer{cfgRef: cfg, sessionMgr: manager}
	srv.cfg.agentName = agentDir
	srv.agentRuntimeFactory = func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		return newServeAgentRuntime(ctx, request, serveAgentRuntimeOptions{cfg: cfg, cmd: &cobra.Command{}, approval: resolvedApprovalMode{Mode: tools.ModePrompt}})
	}
	srv.runtimeFactory = srv.agentRuntimeFactory
	plan := responseModelSwapPlan{enabled: true, previousProvider: "debug", previousModel: "allowed", requestedProvider: "debug", requestedModel: "denied"}
	swap, err := srv.beginResponseModelSwap(context.Background(), "allowlist-swap", plan, nil)
	if swap != nil {
		t.Cleanup(swap.markRolledBack)
	}
	if err == nil || !strings.Contains(err.Error(), "allowed: debug:allowed") || swap != nil {
		t.Fatalf("model swap = %p, error = %v, want allowlist denial", swap, err)
	}
	if current, ok := manager.Get("allowlist-swap"); !ok || current != previous || provider.closed.Load() {
		t.Fatal("disallowed switch changed or closed the original runtime")
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
