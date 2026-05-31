package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestApplyProviderOverridesWithAgentResolvesFastModelAlias(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"openai": {Type: config.ProviderTypeOpenAI},
		},
	}

	if err := applyProviderOverridesWithAgent(cfg, "", "", "", "", "fast"); err != nil {
		t.Fatalf("applyProviderOverridesWithAgent: %v", err)
	}

	if cfg.DefaultProvider != "openai" {
		t.Fatalf("DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}
	if got := cfg.Providers["openai"].Model; got != "gpt-5.4-nano" {
		t.Fatalf("openai model = %q, want gpt-5.4-nano", got)
	}
}

func TestApplyProviderOverridesWithAgentResolvesFastProvider(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "workhorse",
		Providers: map[string]config.ProviderConfig{
			"workhorse": {
				Type:         config.ProviderTypeAnthropic,
				FastProvider: "cheap-openai",
				FastModel:    "gpt-fast-custom",
			},
			"cheap-openai": {Type: config.ProviderTypeOpenAI},
		},
	}

	if err := applyProviderOverridesWithAgent(cfg, "", "", "", "", "fast"); err != nil {
		t.Fatalf("applyProviderOverridesWithAgent: %v", err)
	}

	if cfg.DefaultProvider != "cheap-openai" {
		t.Fatalf("DefaultProvider = %q, want cheap-openai", cfg.DefaultProvider)
	}
	if got := cfg.Providers["cheap-openai"].Model; got != "gpt-fast-custom" {
		t.Fatalf("cheap-openai model = %q, want gpt-fast-custom", got)
	}
}

func TestApplyProviderOverridesWithAgentCLISkipsAgentFastModel(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"openai": {Type: config.ProviderTypeOpenAI},
		},
	}

	if err := applyProviderOverridesWithAgent(cfg, "", "", "anthropic:claude-sonnet-4-6", "", "fast"); err != nil {
		t.Fatalf("applyProviderOverridesWithAgent: %v", err)
	}

	if cfg.DefaultProvider != "anthropic" {
		t.Fatalf("DefaultProvider = %q, want anthropic", cfg.DefaultProvider)
	}
	if got := cfg.Providers["anthropic"].Model; got != "claude-sonnet-4-6" {
		t.Fatalf("anthropic model = %q, want claude-sonnet-4-6", got)
	}
	if got := cfg.Providers["openai"].Model; got != "" {
		t.Fatalf("openai model = %q, want untouched empty model", got)
	}
}

func TestResolveAgentModelOverrideUsesExplicitProviderTypeFallback(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "custom-llm",
		Providers: map[string]config.ProviderConfig{
			"custom-llm": {Type: config.ProviderTypeAnthropic},
		},
	}

	provider, model, resolved, err := resolveAgentModelOverride(cfg, "fast")
	if err != nil {
		t.Fatalf("resolveAgentModelOverride: %v", err)
	}
	if !resolved {
		t.Fatalf("resolved = false, want true")
	}
	if provider != "custom-llm" || model != "claude-haiku-4-5" {
		t.Fatalf("provider/model = %q/%q, want custom-llm/claude-haiku-4-5", provider, model)
	}
}

func TestRegisterModelLimitsRegistersVLLMReasoningEnabled(t *testing.T) {
	defer llm.RegisterConfigReasoningEfforts(nil)
	defer llm.RegisterConfigLimits(nil)
	defer llm.RegisterProviderAliases(nil)

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"cdck_qwen": {
			Type:            config.ProviderTypeVLLM,
			Model:           "Qwen/Qwen3.5-122B-A10B",
			Models:          []string{"Qwen/Qwen3.5-122B-A10B-Instruct"},
			Reasoning:       "enabled",
			ContextWindow:   200000,
			MaxOutputTokens: 50000,
		},
	}}

	registerModelLimits(cfg)

	want := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	for _, model := range []string{"Qwen/Qwen3.5-122B-A10B", "Qwen/Qwen3.5-122B-A10B-Instruct"} {
		got := llm.ReasoningEffortsForProviderModel("cdck_qwen", model)
		if !equalStringSlices(got, want) {
			t.Fatalf("reasoning efforts for %s = %v, want %v", model, got, want)
		}
	}
	base, effort := llm.BaseModelAndEffortForProvider("cdck_qwen", "Qwen/Qwen3.5-122B-A10B-max")
	if base != "Qwen/Qwen3.5-122B-A10B" || effort != "max" {
		t.Fatalf("BaseModelAndEffortForProvider = (%q, %q), want qwen/max", base, effort)
	}
	if got := llm.InputLimitForProviderModel("cdck_qwen", "Qwen/Qwen3.5-122B-A10B-medium"); got != 150000 {
		t.Fatalf("InputLimitForProviderModel suffixed = %d, want 150000", got)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
