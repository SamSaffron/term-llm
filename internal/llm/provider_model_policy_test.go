package llm

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestProviderAllowedModels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		model   string
		want    bool
	}{
		{"omitted", nil, "anything", true},
		{"empty", []string{}, "anything", true},
		{"base", []string{"claude-opus-5-5"}, "claude-opus-5-5", true},
		{"low", []string{"claude-opus-5-5"}, "claude-opus-5-5-low", true},
		{"medium", []string{"claude-opus-5-5"}, "claude-opus-5-5-medium", true},
		{"high", []string{"claude-opus-5-5"}, "claude-opus-5-5-high", true},
		{"xhigh", []string{"claude-opus-5-5"}, "claude-opus-5-5-xhigh", true},
		{"max", []string{"claude-opus-5-5"}, "claude-opus-5-5-max", true},
		{"sonnet", []string{"claude-sonnet-5-5", "claude-opus-5-5"}, "claude-sonnet-5-5-high", true},
		{"old generation", []string{"claude-opus-5-5"}, "claude-opus-4-6-max", false},
		{"moving alias", []string{"claude-opus-5-5"}, "opus-max", false},
		{"other model", []string{"claude-opus-5-5"}, "claude-sonnet-5-5-max", false},
		{"not effort", []string{"claude-opus-5-5"}, "claude-opus-5-5-mini", false},
		{"exact variant", []string{"claude-opus-5-5-high"}, "claude-opus-5-5-high", true},
		{"variant not family", []string{"claude-opus-5-5-high"}, "claude-opus-5-5-max", false},
		{"variant not base", []string{"claude-opus-5-5-high"}, "claude-opus-5-5", false},
		{"case sensitive", []string{"claude-opus-5-5"}, "Claude-opus-5-5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Providers: map[string]config.ProviderConfig{"claude-bin": {AllowedModels: tc.allowed}}}
			err := CheckProviderModelAllowed(cfg, "claude-bin", tc.model)
			if (err == nil) != tc.want {
				t.Fatalf("CheckProviderModelAllowed(%q) = %v, want allowed=%v", tc.model, err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "providers.claude-bin.allowed_models") {
				t.Fatalf("unhelpful error: %v", err)
			}
		})
	}
}

func TestProviderAllowedModelsConfiguredAliasesAndEfforts(t *testing.T) {
	for _, allowed := range []string{"friendly", "upstream/model"} {
		cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
			Type:          config.ProviderTypeOpenAICompat,
			Model:         "friendly",
			AllowedModels: []string{allowed},
			Models:        []string{"friendly"},
			ModelConfigs:  []config.ProviderModelConfig{{ID: "upstream/model", Alias: "friendly", ReasoningEfforts: []string{"high", "max", "custom"}}},
		}}}
		for _, model := range []string{"", "friendly", "friendly-high", "upstream/model-max", "friendly-custom"} {
			if err := CheckProviderModelAllowed(cfg, "custom", model); err != nil {
				t.Errorf("allowed=%q model=%q: %v", allowed, model, err)
			}
		}
		for _, model := range []string{"friendly-low", "upstream/model-low", "unrelated-max"} {
			if err := CheckProviderModelAllowed(cfg, "custom", model); err == nil {
				t.Errorf("allowed=%q accepted %q", allowed, model)
			}
		}
		if err := CheckProviderModelAllowed(cfg, "other", "anything"); err != nil {
			t.Fatal("restriction leaked to another provider:", err)
		}
	}
}

func TestProviderAllowedModelsDeclaredLiteralHasNoEfforts(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
		AllowedModels: []string{"literal"}, Models: []string{"literal"},
		ModelConfigs: []config.ProviderModelConfig{{ID: "literal"}},
	}}}
	if err := CheckProviderModelAllowed(cfg, "custom", "literal-high"); err == nil {
		t.Fatal("declared literal gained undeclared effort")
	}
}

func TestProviderAllowedModelsCreation(t *testing.T) {
	for _, noRetry := range []bool{false, true} {
		cfg := &config.Config{DefaultProvider: "claude-bin", Providers: map[string]config.ProviderConfig{"claude-bin": {
			Model: "opus", AllowedModels: []string{"claude-opus-5-5"},
		}}}
		create := NewProviderByName
		if noRetry {
			create = NewProviderByNameNoRetry
		}
		if _, err := create(cfg, "claude-bin", "claude-opus-5-5-max"); err != nil {
			t.Fatalf("allowed effort (noRetry=%v): %v", noRetry, err)
		}
		for _, model := range []string{"", "opus-max", "claude-sonnet-4-6-high"} {
			if _, err := create(cfg, "claude-bin", model); err == nil || !strings.Contains(err.Error(), "allowed_models") {
				t.Fatalf("model %q bypassed ceiling (noRetry=%v): %v", model, noRetry, err)
			}
		}
		if _, err := NewProvider(cfg); err == nil {
			t.Fatal("default model bypassed ceiling")
		}
	}
}

func TestProviderAllowedModelsRejectBeforeDeferredCredentials(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
		Type: config.ProviderTypeOpenAICompat, Model: "denied", AllowedModels: []string{"allowed"},
		APIKey: "$(this-command-must-not-run)", BaseURL: "https://example.invalid/v1",
	}}}
	if _, err := NewProviderByName(cfg, "custom", "denied"); err == nil || !strings.Contains(err.Error(), "allowed_models") {
		t.Fatalf("expected policy denial before credential resolution: %v", err)
	}
}

func TestProviderAllowedModelsDefaultCannotHideBehindEmptyModel(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"claude-bin": {AllowedModels: []string{"claude-opus-5-5"}}}}
	if err := CheckProviderModelAllowed(cfg, "claude-bin", ""); err == nil {
		t.Fatal("empty model bypassed ceiling via backend default")
	}
}

func TestProviderAllowedModelsAnyProvider(t *testing.T) {
	for _, tc := range []struct {
		provider, base, variant string
		kind                    config.ProviderType
	}{
		{"chatgpt", "gpt-6-sol", "gpt-6-sol-high", ""},
		{"openai", "gpt-5.6-sol", "gpt-5.6-sol-high", ""},
		{"anthropic", "claude-sonnet-4-6", "claude-sonnet-4-6", ""},
		{"ollama", "qwen:8b", "qwen:8b", ""},
		{"router", "upstream/model", "upstream/model-max", config.ProviderTypeOpenAICompat},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			cfg := &config.Config{Providers: map[string]config.ProviderConfig{tc.provider: {Type: tc.kind, Model: tc.base, AllowedModels: []string{tc.base}}}}
			if err := CheckProviderModelAllowed(cfg, tc.provider, tc.variant); err != nil {
				t.Fatal(err)
			}
			if err := CheckProviderModelAllowed(cfg, tc.provider, "unrelated"); err == nil {
				t.Fatal("non-Claude provider restriction ignored")
			}
			if err := CheckProviderModelAllowed(cfg, "other-provider", "unrelated"); err != nil {
				t.Fatal("restriction applied by type instead of provider key", err)
			}
		})
	}
}

// Regression for the requested boundary: effort variants are not model prefixes.
func TestProviderAllowedModelsAmazingModelIsNotAPrefix(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"hypothetical": {AllowedModels: []string{"amazing-model"}}}}
	for _, model := range []string{"amazing-model", "amazing-model-high", "amazing-model-max"} {
		if err := CheckProviderModelAllowed(cfg, "hypothetical", model); err != nil {
			t.Errorf("allowed variant %q: %v", model, err)
		}
	}
	for _, model := range []string{"amazing-model2", "amazing-model2-high", "amazing-model-mini", "amazing-model-high-extra", "amazing-model-high-high"} {
		if err := CheckProviderModelAllowed(cfg, "hypothetical", model); err == nil {
			t.Errorf("base allowlist incorrectly matched prefix %q", model)
		}
	}
}

func TestProviderAllowedModelsPlainDiscoveryListDoesNotNarrowEfforts(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {Type: config.ProviderTypeOpenAICompat, AllowedModels: []string{"amazing-model"}, Models: []string{"amazing-model", "amazing-model2"}}}}
	for _, model := range []string{"amazing-model", "amazing-model-high", "amazing-model-max"} {
		if err := CheckProviderModelAllowed(cfg, "custom", model); err != nil {
			t.Errorf("plain discovery entry narrowed effort allowance %q: %v", model, err)
		}
	}
	if err := CheckProviderModelAllowed(cfg, "custom", "amazing-model2"); err == nil {
		t.Fatal("discovery list widened allowed model")
	}
}
