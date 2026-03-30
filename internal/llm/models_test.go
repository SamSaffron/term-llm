package llm

import "testing"

func TestProviderModelsIncludeGPT54MiniAndNano(t *testing.T) {
	if !containsModelID(ProviderModelIDs("openai"), "gpt-5.4-mini") {
		t.Fatalf("openai models missing gpt-5.4-mini")
	}
	if !containsModelID(ProviderModelIDs("openai"), "gpt-5.4-nano") {
		t.Fatalf("openai models missing gpt-5.4-nano")
	}
	if !containsModelID(ProviderModelIDs("chatgpt"), "gpt-5.4-mini") {
		t.Fatalf("chatgpt models missing gpt-5.4-mini")
	}
	if containsModelID(ProviderModelIDs("chatgpt"), "gpt-5.4-nano") {
		t.Fatalf("chatgpt models unexpectedly include gpt-5.4-nano")
	}
}

func TestProviderFastModelsUseLatestGPT54LightweightModels(t *testing.T) {
	if got := ProviderFastModels["openai"]; got != "gpt-5.4-nano" {
		t.Fatalf("ProviderFastModels[openai] = %q, want %q", got, "gpt-5.4-nano")
	}
	if got := ProviderFastModels["chatgpt"]; got != "gpt-5.4-mini" {
		t.Fatalf("ProviderFastModels[chatgpt] = %q, want %q", got, "gpt-5.4-mini")
	}
}

func TestProviderModelIDs(t *testing.T) {
	ids := ProviderModelIDs("anthropic")
	if len(ids) == 0 {
		t.Fatal("expected non-empty model list for anthropic")
	}
	for _, id := range ids {
		if id == "" {
			t.Error("empty model ID in anthropic list")
		}
	}

	if ids := ProviderModelIDs("nonexistent"); ids != nil {
		t.Errorf("expected nil for unknown provider, got %v", ids)
	}
}

func TestProviderModelIDsMatchEntries(t *testing.T) {
	for provider, entries := range ProviderModels {
		ids := ProviderModelIDs(provider)
		if len(ids) != len(entries) {
			t.Errorf("provider %q: ProviderModelIDs returned %d items, entries has %d",
				provider, len(ids), len(entries))
			continue
		}
		for i, id := range ids {
			if id != entries[i].ID {
				t.Errorf("provider %q index %d: ProviderModelIDs=%q, entry.ID=%q",
					provider, i, id, entries[i].ID)
			}
		}
	}
}

// TestAllListedModelsHaveContextLimits ensures every model in ProviderModels
// resolves to a non-zero input limit via explicit entry, prefix table, or
// config. Models with genuinely unknown limits are exempted.
func TestAllListedModelsHaveContextLimits(t *testing.T) {
	// Models where upstream limits are unknown or not applicable
	exemptions := map[string]bool{
		// Venice models with unknown upstream limits
		"venice-uncensored":                  true,
		"olafangensan-glm-4.7-flash-heretic": true,
		"zai-org-glm-4.7-flash":              true,
		"zai-org-glm-5":                      true,
		"zai-org-glm-4.7":                    true,
		"qwen3-4b":                           true,
		"mistral-small-3-2-24b-instruct":     true,
		"qwen3-235b-a22b-thinking-2507":      true,
		"qwen3-235b-a22b-instruct-2507":      true,
		"qwen3-next-80b":                     true,
		"qwen3-coder-480b-a35b-instruct":     true,
		"qwen3-5-9b":                         true,
		"qwen3-5-35b-a3b":                    true,
		"hermes-3-llama-3.1-405b":            true,
		"google-gemma-3-27b-it":              true,
		"openai-gpt-oss-120b":                true,
		"llama-3.2-3b":                       true,
		"llama-3.3-70b":                      true,
		"minimax-m21":                        true,
		"minimax-m25":                        true,
		"minimax-m27":                        true,
		"qwen3-vl-235b-a22b":                 true,
		// Zen models with unknown limits
		"big-pickle":                 true,
		"glm-4.7-free":               true,
		"trinity-large-preview-free": true,
		"kimi-k2.5-free":             true,
		"minimax-m2.1-free":          true,
		// claude-bin aliases (resolved internally, limits don't apply)
		"opus": true, "opus-low": true, "opus-medium": true, "opus-high": true, "opus-max": true,
		"sonnet": true, "sonnet-low": true, "sonnet-medium": true, "sonnet-high": true,
		"haiku": true,
		// OpenRouter (slash in name, resolved via API cache)
		"x-ai/grok-code-fast-1": true,
		// Copilot models with no known limits
		"raptor-mini": true,
	}

	for provider, entries := range ProviderModels {
		for _, e := range entries {
			if exemptions[e.ID] {
				continue
			}
			limit := InputLimitForProviderModel(provider, e.ID)
			if limit == 0 {
				t.Errorf("provider=%q model=%q has no input limit (add InputLimit to ModelEntry or add to exemptions)", provider, e.ID)
			}
		}
	}
}

func TestVeniceProxyModelsHaveLimits(t *testing.T) {
	tests := []struct {
		model     string
		wantInput int
	}{
		{"claude-opus-4-6", 180_000},
		{"claude-sonnet-4-6", 180_000},
		{"claude-opus-45", 180_000},
		{"claude-sonnet-45", 180_000},
		{"gemini-3-pro-preview", 936_000},
		{"gemini-3-1-pro-preview", 936_000},
		{"gemini-3-flash-preview", 983_000},
		{"grok-41-fast", 1_970_000},
		{"grok-4-20-beta", 192_000},
		{"openai-gpt-54", 922_000},
		{"openai-gpt-54-mini", 272_000},
		{"openai-gpt-52", 272_000},
		{"deepseek-v3.2", 128_000},
		{"grok-code-fast-1", 246_000},
	}
	for _, tt := range tests {
		got := InputLimitForProviderModel("venice", tt.model)
		if got != tt.wantInput {
			t.Errorf("InputLimitForProviderModel(venice, %q) = %d, want %d", tt.model, got, tt.wantInput)
		}
	}
}

func TestVeniceProxyModelsHaveOutputLimits(t *testing.T) {
	tests := []struct {
		model      string
		wantOutput int
	}{
		{"claude-opus-4-6", 64_000},
		{"claude-sonnet-4-6", 64_000},
		{"gemini-3-pro-preview", 65_536},
		{"grok-41-fast", 32_000},
		{"grok-4-20-beta", 64_000},
		{"openai-gpt-54", 128_000},
		{"openai-gpt-54-mini", 128_000},
		{"grok-code-fast-1", 16_384},
	}
	for _, tt := range tests {
		got := OutputLimitForModel(tt.model)
		if got != tt.wantOutput {
			t.Errorf("OutputLimitForModel(%q) = %d, want %d", tt.model, got, tt.wantOutput)
		}
	}
}

func TestProviderAliasResolvesLimits(t *testing.T) {
	// Simulate a custom provider "acme" that is type "venice"
	RegisterProviderAliases(map[string]string{"acme": "venice"})
	defer RegisterProviderAliases(nil)

	// Limits should resolve via "venice" explicit limits
	got := InputLimitForProviderModel("acme", "openai-gpt-54")
	if got != 922_000 {
		t.Errorf("InputLimitForProviderModel(acme, openai-gpt-54) = %d, want 922000", got)
	}

	got = InputLimitForProviderModel("acme", "grok-41-fast")
	if got != 1_970_000 {
		t.Errorf("InputLimitForProviderModel(acme, grok-41-fast) = %d, want 1970000", got)
	}

	// Model discovery should also resolve via alias
	ids := ResolveProviderModelIDs("acme")
	if len(ids) == 0 {
		t.Fatal("ResolveProviderModelIDs(acme) returned empty, expected venice models")
	}
	if !containsModelID(ids, "openai-gpt-54") {
		t.Error("expected openai-gpt-54 in resolved model list for acme")
	}

	// Completions should also work
	completions := GetProviderCompletions("acme:openai-gpt-5", false, nil)
	if len(completions) == 0 {
		t.Error("expected completions for acme:openai-gpt-5, got none")
	}
}

func TestExpandWithEffortVariantsSkipsAlreadySuffixed(t *testing.T) {
	models := []string{"gpt-5.4", "gpt-5.4-high", "gpt-5.2-low", "claude-sonnet-4-6"}
	expanded := ExpandWithEffortVariants(models)

	// gpt-5.4 should get variants
	if !containsModelID(expanded, "gpt-5.4-low") {
		t.Error("expected gpt-5.4-low in expanded list")
	}
	if !containsModelID(expanded, "gpt-5.4-high") {
		t.Error("expected gpt-5.4-high in expanded list")
	}

	// gpt-5.4-high should NOT get nested variants like gpt-5.4-high-low
	if containsModelID(expanded, "gpt-5.4-high-low") {
		t.Error("unexpected nested variant gpt-5.4-high-low")
	}
	if containsModelID(expanded, "gpt-5.2-low-medium") {
		t.Error("unexpected nested variant gpt-5.2-low-medium")
	}

	// claude-sonnet-4-6 should not get variants (not gpt-5 prefix)
	if containsModelID(expanded, "claude-sonnet-4-6-low") {
		t.Error("unexpected variant for non-gpt-5 model")
	}
}

// TestEffortVariantLimitsMatchBase verifies that effort-suffixed models
// (e.g., gpt-5.4-high) resolve to the same limits as their base model in
// ProviderModels via the prefix tables. This catches drift between the two
// data sources.
func TestEffortVariantLimitsMatchBase(t *testing.T) {
	for provider, entries := range ProviderModels {
		for _, e := range entries {
			variants := EffortVariantsFor(e.ID)
			if len(variants) == 0 || e.InputLimit == 0 {
				continue
			}
			for _, v := range variants {
				suffixed := e.ID + "-" + v
				got := InputLimitForProviderModel(provider, suffixed)
				if got == 0 {
					// Variant has no limit at all — prefix table gap
					t.Errorf("provider=%q model=%q (variant of %q): no input limit; prefix table may be missing a catch-all",
						provider, suffixed, e.ID)
				} else if got != e.InputLimit {
					t.Errorf("provider=%q model=%q: input limit %d != base %q limit %d",
						provider, suffixed, got, e.ID, e.InputLimit)
				}
			}
		}
	}
}

func containsModelID(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
