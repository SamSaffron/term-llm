package cmd

import (
	"github.com/samsaffron/term-llm/internal/config"
	"testing"
)

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

func TestApplyAgentModelOverrideAcceptsProviderModelFormat(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "primary",
		Providers: map[string]config.ProviderConfig{
			"primary": {Type: config.ProviderTypeOpenAI, Model: "primary-model"},
			"remote":  {Type: config.ProviderTypeOpenAI},
		},
	}

	if err := applyAgentModelOverride(cfg, "remote:requested-model"); err != nil {
		t.Fatalf("applyAgentModelOverride: %v", err)
	}
	if cfg.DefaultProvider != "remote" {
		t.Fatalf("DefaultProvider = %q, want remote", cfg.DefaultProvider)
	}
	if got := cfg.Providers["remote"].Model; got != "requested-model" {
		t.Fatalf("remote model = %q, want requested-model", got)
	}
	if got := cfg.Providers["primary"].Model; got != "primary-model" {
		t.Fatalf("primary model = %q, want unchanged primary-model", got)
	}
}

func TestApplyAgentModelOverridePreservesUnknownProviderColonModelName(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "local",
		Providers: map[string]config.ProviderConfig{
			"local": {Type: config.ProviderTypeOllama, Model: "old-model"},
		},
	}

	if err := applyAgentModelOverride(cfg, "tagged-model:7b"); err != nil {
		t.Fatalf("applyAgentModelOverride: %v", err)
	}
	if cfg.DefaultProvider != "local" {
		t.Fatalf("DefaultProvider = %q, want local", cfg.DefaultProvider)
	}
	if got := cfg.Providers["local"].Model; got != "tagged-model:7b" {
		t.Fatalf("local model = %q, want tagged-model:7b", got)
	}
}
