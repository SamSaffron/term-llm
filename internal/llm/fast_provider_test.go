package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestProviderFastModelsCoverBuiltIns(t *testing.T) {
	for _, name := range config.GetBuiltInProviderNames() {
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

func TestResolveFastTargetNeverPairsForeignModels(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *config.Config
		provider  string
		wantKey   string
		wantModel string
		wantOK    bool
	}{
		{
			name: "custom endpoint without fast config uses default provider fast target",
			cfg: &config.Config{DefaultProvider: "chatgpt", Providers: map[string]config.ProviderConfig{
				"muse": {Type: config.ProviderTypeOpenAI, BaseURL: "https://api.example.test/v1", Model: "muse-1"},
			}},
			provider: "muse", wantKey: "chatgpt", wantModel: ProviderFastModels["chatgpt"], wantOK: true,
		},
		{
			name: "custom endpoint without default fallback resolves nothing",
			cfg: &config.Config{DefaultProvider: "muse", Providers: map[string]config.ProviderConfig{
				"muse": {Type: config.ProviderTypeOpenAI, BaseURL: "https://api.example.test/v1"},
			}},
			provider: "muse",
		},
		{
			name: "built-in name with a custom base_url gets no built-in fast model",
			cfg: &config.Config{DefaultProvider: "claude-bin", Providers: map[string]config.ProviderConfig{
				"openai": {BaseURL: "https://api.example.test/v1"},
			}},
			provider: "openai", wantKey: "claude-bin", wantModel: ProviderFastModels["claude-bin"], wantOK: true,
		},
		{
			name:     "built-in provider keeps its own fast model",
			cfg:      &config.Config{DefaultProvider: "chatgpt"},
			provider: "claude-bin", wantKey: "claude-bin", wantModel: ProviderFastModels["claude-bin"], wantOK: true,
		},
		{
			name: "custom name on a fixed backend keeps its type fast model",
			cfg: &config.Config{DefaultProvider: "chatgpt", Providers: map[string]config.ProviderConfig{
				"work-claude": {Type: config.ProviderTypeClaudeBin},
			}},
			provider: "work-claude", wantKey: "work-claude", wantModel: ProviderFastModels["claude-bin"], wantOK: true,
		},
		{
			name: "fast_provider redirect uses the target's model, not the source's",
			cfg: &config.Config{DefaultProvider: "chatgpt", Providers: map[string]config.ProviderConfig{
				"chatgpt": {FastProvider: "claude-bin"},
			}},
			provider: "chatgpt", wantKey: "claude-bin", wantModel: ProviderFastModels["claude-bin"], wantOK: true,
		},
		{
			name: "fast_provider redirect uses the target's configured fast model",
			cfg: &config.Config{DefaultProvider: "chatgpt", Providers: map[string]config.ProviderConfig{
				"muse":  {Type: config.ProviderTypeOpenAI, BaseURL: "https://api.example.test/v1", FastProvider: "small"},
				"small": {Type: config.ProviderTypeOpenAI, BaseURL: "https://small.example.test/v1", FastModel: "tiny"},
			}},
			provider: "muse", wantKey: "small", wantModel: "tiny", wantOK: true,
		},
		{
			name: "explicit fast model stays on its provider",
			cfg: &config.Config{DefaultProvider: "chatgpt", Providers: map[string]config.ProviderConfig{
				"muse": {Type: config.ProviderTypeOpenAI, BaseURL: "https://api.example.test/v1", FastModel: "muse-small"},
			}},
			provider: "muse", wantKey: "muse", wantModel: "muse-small", wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, model, ok := ResolveFastTarget(tt.cfg, tt.provider)
			if key != tt.wantKey || model != tt.wantModel || ok != tt.wantOK {
				t.Fatalf("ResolveFastTarget(%q) = %s:%s ok=%v, want %s:%s ok=%v", tt.provider, key, model, ok, tt.wantKey, tt.wantModel, tt.wantOK)
			}
		})
	}
}

func TestResolveFastTargetFallsBackToConfiguredDefaultProvider(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "chatgpt", Providers: map[string]config.ProviderConfig{
		"muse": {Type: config.ProviderTypeOpenAI, BaseURL: "https://api.example.test/v1"},
	}}
	cfg.ApplyOverrides("muse", "muse-1")
	key, model, ok := ResolveFastTarget(cfg, "muse")
	if !ok || key != "chatgpt" || model != ProviderFastModels["chatgpt"] {
		t.Fatalf("ResolveFastTarget(muse) = %s:%s ok=%v, want chatgpt fast target", key, model, ok)
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
