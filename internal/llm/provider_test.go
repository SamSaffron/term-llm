package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestParseProviderModel(t *testing.T) {
	// Create a config with some custom providers
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"anthropic":  {Model: "claude-sonnet-4-5"},
			"openai":     {Model: "gpt-5.2"},
			"gemini":     {Model: "gemini-3-flash-preview"},
			"openrouter": {Model: "x-ai/grok-code-fast-1"},
			"zen":        {Model: "glm-4.7-free"},
			"cerebras": {
				Type:    config.ProviderTypeOpenAICompat,
				BaseURL: "https://api.cerebras.ai/v1",
				Model:   "llama-4-scout-17b",
			},
		},
	}

	tests := []struct {
		name         string
		input        string
		wantProvider string
		wantModel    string
		wantErr      bool
	}{
		{name: "provider only", input: "gemini", wantProvider: "gemini"},
		{name: "provider with model", input: "openai:gpt-4o", wantProvider: "openai", wantModel: "gpt-4o"},
		{name: "openrouter with model", input: "openrouter:x-ai/grok-code-fast-1", wantProvider: "openrouter", wantModel: "x-ai/grok-code-fast-1"},
		{name: "custom provider", input: "cerebras:llama-4-scout-17b", wantProvider: "cerebras", wantModel: "llama-4-scout-17b"},
		{name: "invalid provider", input: "unknown:model", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider, model, err := ParseProviderModel(tc.input, cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider != tc.wantProvider {
				t.Fatalf("provider=%q, want %q", provider, tc.wantProvider)
			}
			if model != tc.wantModel {
				t.Fatalf("model=%q, want %q", model, tc.wantModel)
			}
		})
	}
}
