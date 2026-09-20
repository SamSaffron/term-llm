package llm

import "testing"

func TestHelperInputLimitForSelfManagedProviders(t *testing.T) {
	// These are pinned, not read back from the table under test: a helper budget
	// that silently halves would still trim a real transcript to nothing.
	tests := []struct {
		name         string
		providerName string
		model        string
		want         int
	}{
		{"claude-bin bare alias", "claude-bin", "opus", 160_000},
		{"claude-bin effort suffix", "claude-bin", "opus-max", 160_000},
		{"claude-bin fable suffix", "claude-bin", "fable-medium", 160_000},
		{"claude-bin haiku", "claude-bin", "haiku", 160_000},
		{"cursor-bin routed model", "cursor-bin", "auto-smart", 120_000},
		{"agy-bin model", "agy-bin", "gemini-3.8-flash-high", 160_000},
		{"grok-bin keeps its published limit", "grok-bin", "grok-4.6", 192_000},
		{"unknown non-managed provider stays zero", "unknown-provider", "unknown-model", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HelperInputLimitForProviderModel(tc.providerName, tc.model); got != tc.want {
				t.Fatalf("HelperInputLimitForProviderModel(%q, %q) = %d, want %d", tc.providerName, tc.model, got, tc.want)
			}
		})
	}
	if limit := InputLimitForProviderModel("grok-bin", "grok-4.6"); limit != 192_000 {
		t.Fatalf("test setup: grok-bin published input limit = %d, want the table value the helper budget defers to", limit)
	}
}

// The helper budget must not leak into the surfaces that intentionally report no
// limit for context-managing providers: the context meter, /models, the web
// context-usage endpoints and compaction configuration all read this value.
func TestInputLimitForProviderModelStaysAbsentForClaudeBin(t *testing.T) {
	for _, model := range []string{"opus", "opus-max", "sonnet", "fable-medium", "haiku"} {
		if got := InputLimitForProviderModel("claude-bin", model); got != 0 {
			t.Fatalf("InputLimitForProviderModel(claude-bin, %q) = %d, want 0", model, got)
		}
	}
}
