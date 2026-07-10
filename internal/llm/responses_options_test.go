package llm

import "testing"

func TestMergeResponsesOptionsEphemeralUsesOnlyExplicitOptions(t *testing.T) {
	defaults := ResponsesOptions{
		ReasoningMode:    "pro",
		ReasoningContext: "all_turns",
		MultiAgent: MultiAgentOptions{
			Enabled:                true,
			EnabledSet:             true,
			MaxConcurrentSubagents: 8,
		},
		ProgrammaticToolCalling: ProgrammaticToolCallingOptions{
			Enabled:    true,
			EnabledSet: true,
			Tools:      []string{"shell"},
		},
		PromptCache: PromptCacheOptions{Mode: "explicit", TTL: "30m"},
	}
	override := &ResponsesOptions{ReasoningContext: "current_turn"}

	got := mergeResponsesOptions(defaults, override, true)
	if got.ReasoningMode != "" {
		t.Fatalf("ephemeral ReasoningMode = %q, want empty provider default", got.ReasoningMode)
	}
	if got.ReasoningContext != "current_turn" {
		t.Fatalf("ephemeral ReasoningContext = %q, want explicit override", got.ReasoningContext)
	}
	if got.MultiAgent.Enabled || got.MultiAgent.EnabledSet || got.MultiAgent.MaxConcurrentSubagents != 0 {
		t.Fatalf("ephemeral MultiAgent inherited provider defaults: %+v", got.MultiAgent)
	}
	if got.ProgrammaticToolCalling.Enabled || got.ProgrammaticToolCalling.EnabledSet || len(got.ProgrammaticToolCalling.Tools) != 0 {
		t.Fatalf("ephemeral ProgrammaticToolCalling inherited provider defaults: %+v", got.ProgrammaticToolCalling)
	}
	if got.PromptCache != (PromptCacheOptions{}) {
		t.Fatalf("ephemeral PromptCache inherited provider defaults: %+v", got.PromptCache)
	}
}

func TestValidateResponsesOptionsGPT56ProviderGating(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		wantErr  bool
	}{
		{name: "OpenAI Sol", provider: "openai", model: "gpt-5.6-sol", wantErr: false},
		{name: "OpenAI variant effort suffix", provider: "openai", model: "gpt-5.6-terra-max", wantErr: false},
		{name: "older OpenAI", provider: "openai", model: "gpt-5.5", wantErr: true},
		{name: "ChatGPT Ultra is not Pro", provider: "chatgpt", model: "gpt-5.6-sol-ultra", wantErr: true},
		{name: "compatible provider", provider: "openai-compatible", model: "gpt-5.6-sol", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateResponsesOptions(tt.provider, tt.model, &ResponsesOptions{ReasoningMode: "pro"}, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateResponsesOptions(%q, %q) error = %v, wantErr %t", tt.provider, tt.model, err, tt.wantErr)
			}
		})
	}
}

func TestValidateResponsesOptionsNormalizesAndDefaults(t *testing.T) {
	opts, err := validateResponsesOptions("openai", "gpt-5.6-sol", &ResponsesOptions{
		ReasoningMode:    " Pro ",
		ReasoningContext: " ALL_TURNS ",
		MultiAgent:       MultiAgentOptions{Enabled: true, EnabledSet: true},
		PromptCache:      PromptCacheOptions{Mode: " EXPLICIT ", TTL: " 30M "},
	}, nil)
	if err != nil {
		t.Fatalf("validateResponsesOptions() error = %v", err)
	}
	if opts.ReasoningMode != "pro" || opts.ReasoningContext != "all_turns" {
		t.Fatalf("reasoning options = %q/%q", opts.ReasoningMode, opts.ReasoningContext)
	}
	if opts.MultiAgent.MaxConcurrentSubagents != 3 {
		t.Fatalf("MaxConcurrentSubagents = %d, want 3", opts.MultiAgent.MaxConcurrentSubagents)
	}
	if opts.PromptCache.Mode != "explicit" || opts.PromptCache.TTL != "30m" {
		t.Fatalf("prompt cache options = %+v", opts.PromptCache)
	}
}
