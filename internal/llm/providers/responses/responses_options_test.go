package responses

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
