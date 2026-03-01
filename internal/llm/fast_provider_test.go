package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestProviderFastModelsCoverBuiltIns(t *testing.T) {
	for _, name := range GetBuiltInProviderNames() {
		if ProviderFastModels[name] == "" {
			t.Fatalf("missing fast model for provider %q", name)
		}
	}
}

func TestNewFastProvider(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"claude-bin": {
				Model: "sonnet",
			},
			"my-openai": {
				Type:         config.ProviderTypeOpenAI,
				APIKey:       "sk-test",
				Model:        "gpt-5.2",
				FastProvider: "debug",
				FastModel:    "random",
			},
		},
	}

	fast, err := NewFastProvider(cfg, "claude-bin")
	if err != nil {
		t.Fatalf("NewFastProvider(claude-bin) returned error: %v", err)
	}
	if fast == nil {
		t.Fatalf("expected non-nil fast provider for claude-bin")
	}
	if fast.Name() == "" {
		t.Fatalf("fast provider name should not be empty")
	}

	fast, err = NewFastProvider(cfg, "my-openai")
	if err != nil {
		t.Fatalf("NewFastProvider(my-openai) returned error: %v", err)
	}
	if fast == nil {
		t.Fatalf("expected non-nil fast provider for my-openai")
	}
	if fast.Name() == "" || fast.Name() == "my-openai" {
		t.Fatalf("fast provider name = %q, expected debug provider", fast.Name())
	}
}

func TestNewFastProviderMissing(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"custom": {
			Type: config.ProviderTypeOpenAICompat,
		},
	}}
	fast, err := NewFastProvider(cfg, "custom")
	if err != nil {
		t.Fatalf("NewFastProvider(custom) returned error: %v", err)
	}
	if fast != nil {
		t.Fatalf("expected nil fast provider for custom provider with no fast model")
	}
}
