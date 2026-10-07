package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestCheckAgentModelResolvesFastAlias(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "primary", Providers: map[string]config.ProviderConfig{
		"primary": {Model: "main", FastProvider: "small", FastModel: "quick"},
		"small":   {Model: "quick"},
	}}
	agent := &agents.Agent{Name: "reviewer", AllowedModels: []string{"small:quick"}}
	if err := func() error {
		_, _, err := selectRunModel(cfg, modelSelectionInput{Agent: agent, Fast: true})
		return err
	}(); err != nil {
		t.Fatalf("fast model should be allowed: %v", err)
	}
	if err := func() error { _, _, err := selectRunModel(cfg, modelSelectionInput{Agent: agent}); return err }(); err == nil {
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
			if err := func() error {
				_, _, err := selectRunModel(cfg, modelSelectionInput{Agent: agent, ProviderFlag: selection})
				return err
			}(); err == nil || !strings.Contains(err.Error(), "allowed: debug:allowed") {
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

func TestSelectRunModelInheritedPolicyAndLiveParent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agent    *agents.Agent
		override string
		parent   runpkg.ParentModel
		allowed  bool
		want     string
	}{
		{name: "inherit live selection", agent: &agents.Agent{Name: "child"}, parent: runpkg.ParentModel{Provider: "debug", Model: "live"}, allowed: true, want: "live"},
		{name: "own conflict", agent: &agents.Agent{Name: "child", AllowedModels: []string{"other:*"}}, parent: runpkg.ParentModel{Provider: "debug", Model: "live"}},
		{name: "child explicit conflict", agent: &agents.Agent{Name: "child", Provider: "other", Model: "denied"}, parent: runpkg.ParentModel{Provider: "debug", Model: "live"}},
		{name: "request override conflict", agent: &agents.Agent{Name: "child"}, override: "other:denied", parent: runpkg.ParentModel{Provider: "debug", Model: "live"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{DefaultProvider: "other", Providers: map[string]config.ProviderConfig{"debug": {Model: "base"}, "other": {Model: "denied"}}}
			selected, _, err := selectRunModel(cfg, modelSelectionInput{Agent: tc.agent, ModelOverride: tc.override, Inherited: modelpolicy.Policy{}.With("boss", []string{"debug:*"}), Parent: tc.parent})
			if tc.allowed {
				if err != nil || selected.Provider != "debug" || selected.Model != tc.want {
					t.Fatalf("selected=%+v err=%v", selected, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "parent agent \"boss\"") && tc.name != "own conflict" {
				t.Fatalf("want parent denial, got %v", err)
			}
		})
	}
}

func TestSpawnAdmissionDenialPrecedesStartedEvent(t *testing.T) {
	cfg, _ := modelAllowlistFixture(t, "debug:allowed")
	runner, err := NewSpawnAgentRunner(cfg, false, tools.NewApprovalManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	runner.SetModelPolicySource(func() ParentModelState {
		return ParentModelState{Policy: modelpolicy.Policy{}.With("boss", []string{"debug:*"}), Provider: "debug", Model: "allowed"}
	})
	events := 0
	_, err = runner.runChildInternal(context.Background(), runpkg.ChildRunRequest{AgentName: "reviewer", ModelOverride: "other:denied", Prompt: "test"}, func(string, tools.SubagentEvent) { events++ })
	var admission *tools.AgentRunAdmissionError
	if !errors.As(err, &admission) || events != 0 || !strings.Contains(err.Error(), "boss") {
		t.Fatalf("admission=%v events=%d", err, events)
	}
}

func TestResumedChildKeepsPersistedModel(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "new-default"}}}
	selected, _, err := selectRunModel(cfg, modelSelectionInput{Agent: &agents.Agent{Name: "child", Provider: "debug", Model: "changed-default"}, Inherited: modelpolicy.Policy{}.With("boss", []string{"debug:previous"}), Parent: runpkg.ParentModel{Provider: "debug", Model: "parent-live"}, ResumeModel: runpkg.ParentModel{Provider: "debug", Model: "previous"}})
	if err != nil || selected.Model != "previous" {
		t.Fatalf("resumed selection=%+v err=%v", selected, err)
	}
}

func TestRunnerFastSelectionUsesCheckedTarget(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "primary", Providers: map[string]config.ProviderConfig{"primary": {Model: "main", FastProvider: "debug", FastModel: "small"}, "debug": {Model: "small"}}}
	runner := &cmdRunner{baseCfg: cfg, defaults: cmdRunnerOptions{Fast: true, ConfigSet: true}}
	selectedCfg, _, selection, _, err := runner.resolveRunModel(context.Background(), runpkg.Request{})
	if err != nil || selection.Provider != "debug" || selection.Model != "small" || selectedCfg.DefaultProvider != "debug" || activeModel(selectedCfg) != "small" {
		t.Fatalf("cfg=%+v selected=%+v err=%v", selectedCfg, selection, err)
	}
}

func TestContinueAdmissionDenialPreservesCompletedChild(t *testing.T) {
	cfg, _ := modelAllowlistFixture(t, "debug:allowed")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	child := &session.Session{ID: session.NewID(), Provider: "debug", ProviderKey: "debug", Model: "allowed", Agent: "reviewer", Status: session.StatusComplete}
	if err := store.Create(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	runner, err := NewSpawnAgentRunnerWithStore(cfg, false, tools.NewApprovalManager(nil), store, "parent")
	if err != nil {
		t.Fatal(err)
	}
	runner.SetModelPolicySource(func() ParentModelState {
		return ParentModelState{Policy: modelpolicy.Policy{}.With("boss", []string{"debug:*"}), Provider: "debug", Model: "allowed"}
	})
	count := 0
	_, err = runner.runChildInternal(context.Background(), runpkg.ChildRunRequest{AgentName: "reviewer", ChildSessionID: child.ID, Resume: true, ModelOverride: "other:denied"}, func(string, tools.SubagentEvent) { count++ })
	var admission *tools.AgentRunAdmissionError
	if !errors.As(err, &admission) || count != 0 {
		t.Fatalf("admission=%v events=%d", err, count)
	}
	saved, err := store.Get(context.Background(), child.ID)
	if err != nil || saved.Status != session.StatusComplete {
		t.Fatalf("child status=%+v err=%v", saved, err)
	}
}

func TestResolveRunModelKeepsInheritedRuleIdenticalToOwnRule(t *testing.T) {
	cfg, agentDir := modelAllowlistFixture(t, "debug:allowed")
	parent := modelpolicy.Policy{}.With("reviewer", []string{"debug:allowed"})
	runner := &cmdRunner{baseCfg: cfg}
	_, _, _, policies, err := runner.resolveRunModel(context.Background(), runpkg.Request{AgentName: agentDir, ModelPolicy: parent})
	if err != nil {
		t.Fatal(err)
	}
	if len(policies.Inherited.Rules) != 1 || policies.Inherited.Rules[0].Agent != "reviewer" {
		t.Fatalf("inherited rules = %+v, want the parent's rule preserved", policies.Inherited.Rules)
	}
}

func TestSpawnedChildPersistsInheritedPolicy(t *testing.T) {
	cfg, _ := modelAllowlistFixture(t, "debug:allowed")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parentSess := &session.Session{ID: session.NewID(), Provider: "debug", ProviderKey: "debug", Model: "allowed", Agent: "boss"}
	if err := store.Create(context.Background(), parentSess); err != nil {
		t.Fatal(err)
	}
	runner, err := NewSpawnAgentRunnerWithStore(cfg, false, tools.NewApprovalManager(nil), store, parentSess.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Wait()
	boss := modelpolicy.Policy{}.With("boss", []string{"debug:*"})
	runner.SetModelPolicySource(func() ParentModelState {
		return ParentModelState{Policy: boss, Provider: "debug", Model: "allowed"}
	})
	childID := session.NewID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runner.runChildInternal(ctx, runpkg.ChildRunRequest{AgentName: "reviewer", ChildSessionID: childID, Prompt: "hello"}, nil); err != nil {
		t.Fatalf("child run: %v", err)
	}
	saved, err := store.Get(context.Background(), childID)
	if err != nil || saved == nil {
		t.Fatalf("child session = %+v, err = %v", saved, err)
	}
	if len(saved.ModelPolicy.Rules) != 1 || saved.ModelPolicy.Rules[0].Agent != "boss" {
		t.Fatalf("persisted policy = %+v, want only the inherited boss rule", saved.ModelPolicy)
	}
}
