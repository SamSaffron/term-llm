package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

// writePromptLLMAgent writes an agent whose preferred provider differs from the
// one a request will select, with a prompt that renders the active LLM.
func writePromptLLMAgent(t *testing.T) (string, *config.Config) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	agentDir := filepath.Join(root, "llm-probe")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: llm-probe\nprovider: preferred\nmodel: preferred-model\nskills: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "system.md"), []byte("llm={{provider_model}} provider={{provider}} model={{model}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DefaultProvider: "preferred",
		Providers: map[string]config.ProviderConfig{
			"preferred": {Model: "preferred-model"},
			"selected":  {Model: "selected-default"},
		},
		Agents: config.AgentsConfig{SearchPaths: []string{root}},
	}
	return "llm-probe", cfg
}

func TestCmdRunnerPreparePromptUsesSelectedProviderAndModel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		// The web UI sends the provider and the model separately. Before the fix
		// the model was honoured but the agent's preferred provider leaked into
		// the prompt, rendering "preferred:big".
		{name: "provider and model", provider: "selected", model: "big", want: "llm=selected:big provider=selected model=big"},
		{name: "provider only", provider: "selected", want: "llm=selected:selected-default provider=selected model=selected-default"},
		{name: "provider:model flag", provider: "selected:big", want: "llm=selected:big provider=selected model=big"},
		{name: "agent preference", want: "llm=preferred:preferred-model provider=preferred model=preferred-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentName, cfg := writePromptLLMAgent(t)
			runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
			env, err := runner.prepare(context.Background(), runpkg.Request{
				Platform:         runpkg.PlatformWeb,
				AgentName:        agentName,
				Provider:         tc.provider,
				Model:            tc.model,
				ProviderInstance: llm.NewMockProvider("mock"),
				Cwd:              t.TempDir(),
				DeferSession:     true,
			}, nil)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			defer env.Close()
			if !strings.Contains(env.settings.SystemPrompt, tc.want) {
				t.Fatalf("SystemPrompt = %q, want it to contain %q", env.settings.SystemPrompt, tc.want)
			}
		})
	}
}

func TestResolveSettingsActiveLLMWinsOverAgentPreference(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "term-llm"), 0o755); err != nil {
		t.Fatal(err)
	}
	userAgents := "shared rule\n\n[[[claude-bin]]]\nclaude-bin only rule\n"
	if err := os.WriteFile(filepath.Join(xdg, "term-llm", "AGENTS.md"), []byte(userAgents), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := &agents.Agent{Name: "probe", Provider: "chatgpt", Model: "gpt-pref", AgentsMd: "true", SystemPrompt: "llm={{provider_model}}"}

	for _, tc := range []struct {
		name           string
		activeProvider string
		activeModel    string
		wantLLM        string
		wantGated      bool
	}{
		{name: "selected provider", activeProvider: "claude-bin", activeModel: "opus", wantLLM: "llm=claude-bin:opus", wantGated: true},
		{name: "no active selection keeps agent preference", wantLLM: "llm=chatgpt:gpt-pref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, err := ResolveSettingsInDir(&config.Config{}, agent, CLIFlags{ActiveProvider: tc.activeProvider, ActiveModel: tc.activeModel}, "", "", "", 0, 10, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(settings.SystemPrompt, tc.wantLLM) {
				t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, tc.wantLLM)
			}
			if !strings.Contains(settings.SystemPrompt, "shared rule") {
				t.Fatalf("SystemPrompt lost ungated AGENTS.md content: %q", settings.SystemPrompt)
			}
			if got := strings.Contains(settings.SystemPrompt, "claude-bin only rule"); got != tc.wantGated {
				t.Fatalf("gated block included = %v, want %v; prompt %q", got, tc.wantGated, settings.SystemPrompt)
			}
		})
	}
}
