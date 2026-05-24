package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
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
