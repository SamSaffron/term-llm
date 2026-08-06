package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestParseProviderModel(t *testing.T) {
	// Create a config with some custom providers
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"anthropic":  {Model: "claude-sonnet-4-6"},
			"openai":     {Model: "gpt-5.2"},
			"gemini":     {Model: "gemini-3-flash-preview"},
			"openrouter": {Model: "x-ai/grok-code-fast-1"},
			"zen":        {Model: "minimax-m2.5-free"},
			"cerebras": {
				Type:    config.ProviderTypeOpenAICompat,
				BaseURL: "https://api.cerebras.ai/v1",
				Model:   "llama-4-scout-17b",
			},
			"cdck_qwen": {
				Type:    config.ProviderTypeVLLM,
				BaseURL: "https://example.test/v1",
				Model:   "Qwen/Qwen3.5-122B-A10B",
			},
			"cdck_deepseek": {
				Type:  config.ProviderTypeVLLM,
				Model: "deepseek-ai/DeepSeek-V4-Flash",
				ModelConfigs: []config.ProviderModelConfig{{
					ID:                     "deepseek-ai/DeepSeek-V4-Flash",
					Alias:                  "deepseek-v4-flash",
					ReasoningEfforts:       []string{"none", "low", "high", "max"},
					DefaultReasoningEffort: "high",
				}},
			},
			"suffix_default": {
				Type:  config.ProviderTypeVLLM,
				Model: "flash-xhigh",
				ModelConfigs: []config.ProviderModelConfig{{
					ID:               "vendor/flash",
					Alias:            "flash",
					ReasoningEfforts: []string{"high", "xhigh", "low"},
				}},
			},
			"natural_suffix": {
				Type:  config.ProviderTypeVLLM,
				Model: "vendor/model-xhigh",
				ModelConfigs: []config.ProviderModelConfig{{
					ID:               "vendor/model-xhigh",
					ReasoningEfforts: []string{"high", "xhigh", "low"},
				}},
			},
			"claude": {
				Type:  config.ProviderTypeVLLM,
				Model: "vendor/model",
				ModelConfigs: []config.ProviderModelConfig{{
					ID:               "vendor/model",
					ReasoningEfforts: []string{"bin", "high"},
				}},
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
		{name: "configured provider with effort suffix", input: "cdck_qwen-high", wantProvider: "cdck_qwen", wantModel: "Qwen/Qwen3.5-122B-A10B-high"},
		{name: "configured provider explicit none effort", input: "cdck_deepseek-none", wantProvider: "cdck_deepseek", wantModel: "deepseek-ai/DeepSeek-V4-Flash-none"},
		{name: "configured provider explicit max effort", input: "cdck_deepseek-max", wantProvider: "cdck_deepseek", wantModel: "deepseek-ai/DeepSeek-V4-Flash-max"},
		{name: "configured provider rejects unadvertised effort", input: "cdck_deepseek-medium", wantErr: true},
		{name: "configured default suffix trims longest match", input: "suffix_default-low", wantProvider: "suffix_default", wantModel: "flash-low"},
		{name: "natural model suffix is preserved", input: "natural_suffix-low", wantProvider: "natural_suffix", wantModel: "vendor/model-xhigh-low"},
		{name: "built-in provider wins over configured prefix", input: "claude-bin", wantProvider: "claude-bin"},
		{name: "configured provider effort suffix replaces model suffix", input: "openai-low", wantProvider: "openai", wantModel: "gpt-5.2-low"},
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
