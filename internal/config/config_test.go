package config

import "testing"

func TestApplyOverrides(t *testing.T) {
	cfg := &Config{
		DefaultProvider: "anthropic",
		Providers: map[string]ProviderConfig{
			"anthropic": {
				Model: "claude-sonnet-4-5",
			},
			"openai": {
				Model: "gpt-5.2",
			},
			"gemini": {
				Model: "gemini-3-flash-preview",
			},
		},
	}

	cfg.ApplyOverrides("openai", "gpt-4o")
	if cfg.DefaultProvider != "openai" {
		t.Fatalf("provider=%q, want %q", cfg.DefaultProvider, "openai")
	}
	if cfg.Providers["openai"].Model != "gpt-4o" {
		t.Fatalf("openai model=%q, want %q", cfg.Providers["openai"].Model, "gpt-4o")
	}
	if cfg.Providers["anthropic"].Model != "claude-sonnet-4-5" {
		t.Fatalf("anthropic model changed unexpectedly: %q", cfg.Providers["anthropic"].Model)
	}

	cfg.ApplyOverrides("", "gemini-2.5-flash")
	if cfg.DefaultProvider != "openai" {
		t.Fatalf("provider changed unexpectedly: %q", cfg.DefaultProvider)
	}
	if cfg.Providers["openai"].Model != "gemini-2.5-flash" {
		t.Fatalf("openai model=%q, want %q", cfg.Providers["openai"].Model, "gemini-2.5-flash")
	}
}

func TestInferProviderType(t *testing.T) {
	tests := []struct {
		name     string
		explicit ProviderType
		want     ProviderType
	}{
		{"anthropic", "", ProviderTypeAnthropic},
		{"openai", "", ProviderTypeOpenAI},
		{"gemini", "", ProviderTypeGemini},
		{"openrouter", "", ProviderTypeOpenRouter},
		{"zen", "", ProviderTypeZen},
		{"cerebras", "", ProviderTypeOpenAICompat},
		{"groq", "", ProviderTypeOpenAICompat},
		{"custom", ProviderTypeOpenAICompat, ProviderTypeOpenAICompat},
		{"anthropic", ProviderTypeOpenAICompat, ProviderTypeOpenAICompat}, // explicit overrides
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := InferProviderType(tc.name, tc.explicit)
			if got != tc.want {
				t.Errorf("InferProviderType(%q, %q) = %q, want %q", tc.name, tc.explicit, got, tc.want)
			}
		})
	}
}
