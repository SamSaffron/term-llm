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
		"qwen3-4b": true,
		// Zen models — all have explicit limits now
		// claude-bin aliases (resolved internally, limits don't apply)
		"opus": true, "opus-low": true, "opus-medium": true, "opus-high": true, "opus-xhigh": true, "opus-max": true,
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
		{"venice-uncensored", 32_000},
		{"venice-uncensored-1-2", 128_000},
		{"venice-uncensored-role-play", 128_000},
		{"olafangensan-glm-4.7-flash-heretic", 200_000},
		{"zai-org-glm-4.7-flash", 128_000},
		{"zai-org-glm-5", 198_000},
		{"zai-org-glm-5-1", 200_000},
		{"z-ai-glm-5-turbo", 200_000},
		{"z-ai-glm-5v-turbo", 200_000},
		{"zai-org-glm-4.6", 198_000},
		{"zai-org-glm-4.7", 198_000},
		{"mistral-small-3-2-24b-instruct", 256_000},
		{"mistral-small-2603", 256_000},
		{"qwen3-235b-a22b-thinking-2507", 128_000},
		{"qwen3-235b-a22b-instruct-2507", 128_000},
		{"qwen3-next-80b", 256_000},
		{"qwen3-coder-480b-a35b-instruct", 256_000},
		{"qwen3-coder-480b-a35b-instruct-turbo", 256_000},
		{"qwen-3-6-plus", 1_000_000},
		{"qwen3-5-9b", 256_000},
		{"qwen3-5-35b-a3b", 256_000},
		{"qwen3-5-397b-a17b", 128_000},
		{"hermes-3-llama-3.1-405b", 128_000},
		{"google-gemma-3-27b-it", 198_000},
		{"google-gemma-4-26b-a4b-it", 256_000},
		{"google-gemma-4-31b-it", 256_000},
		{"gemma-4-uncensored", 256_000},
		{"grok-41-fast", 1_000_000},
		{"grok-4-20", 2_000_000},
		{"grok-4-20-multi-agent", 2_000_000},
		{"gemini-3-1-pro-preview", 1_000_000},
		{"gemini-3-flash-preview", 256_000},
		{"claude-opus-4-7", 1_000_000},
		{"claude-opus-4-6", 1_000_000},
		{"claude-opus-4-6-fast", 1_000_000},
		{"claude-opus-4-5", 198_000},
		{"claude-sonnet-4-6", 1_000_000},
		{"claude-sonnet-4-5", 198_000},
		{"openai-gpt-oss-120b", 128_000},
		{"openai-gpt-52", 256_000},
		{"openai-gpt-52-codex", 256_000},
		{"openai-gpt-53-codex", 400_000},
		{"openai-gpt-54", 1_000_000},
		{"openai-gpt-54-mini", 400_000},
		{"openai-gpt-54-pro", 1_000_000},
		{"openai-gpt-4o-2024-11-20", 128_000},
		{"openai-gpt-4o-mini-2024-07-18", 128_000},
		{"kimi-k2-thinking", 256_000},
		{"arcee-trinity-large-thinking", 256_000},
		{"kimi-k2-5", 256_000},
		{"deepseek-v3.2", 160_000},
		{"aion-labs-aion-2-0", 128_000},
		{"llama-3.2-3b", 128_000},
		{"llama-3.3-70b", 128_000},
		{"minimax-m25", 198_000},
		{"minimax-m27", 198_000},
		{"mercury-2", 128_000},
		{"qwen3-vl-235b-a22b", 256_000},
		{"nvidia-nemotron-3-nano-30b-a3b", 128_000},
		{"nvidia-nemotron-cascade-2-30b-a3b", 256_000},
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
		{"venice-uncensored", 8_192},
		{"venice-uncensored-1-2", 8_192},
		{"venice-uncensored-role-play", 4_096},
		{"olafangensan-glm-4.7-flash-heretic", 24_000},
		{"zai-org-glm-4.7-flash", 16_384},
		{"zai-org-glm-5", 32_000},
		{"zai-org-glm-5-1", 24_000},
		{"z-ai-glm-5-turbo", 32_768},
		{"z-ai-glm-5v-turbo", 32_768},
		{"zai-org-glm-4.6", 16_384},
		{"zai-org-glm-4.7", 16_384},
		{"mistral-small-3-2-24b-instruct", 16_384},
		{"mistral-small-2603", 65_536},
		{"qwen3-235b-a22b-thinking-2507", 16_384},
		{"qwen3-235b-a22b-instruct-2507", 16_384},
		{"qwen3-next-80b", 16_384},
		{"qwen3-coder-480b-a35b-instruct", 65_536},
		{"qwen3-coder-480b-a35b-instruct-turbo", 65_536},
		{"qwen-3-6-plus", 65_536},
		{"qwen3-5-9b", 32_768},
		{"qwen3-5-35b-a3b", 65_536},
		{"qwen3-5-397b-a17b", 32_768},
		{"hermes-3-llama-3.1-405b", 16_384},
		{"google-gemma-3-27b-it", 16_384},
		{"google-gemma-4-26b-a4b-it", 8_192},
		{"google-gemma-4-31b-it", 8_192},
		{"gemma-4-uncensored", 8_192},
		{"grok-41-fast", 30_000},
		{"grok-4-20", 128_000},
		{"grok-4-20-multi-agent", 128_000},
		{"gemini-3-1-pro-preview", 32_768},
		{"gemini-3-flash-preview", 65_536},
		{"claude-opus-4-7", 128_000},
		{"claude-opus-4-6", 128_000},
		{"claude-opus-4-6-fast", 128_000},
		{"claude-opus-4-5", 32_768},
		{"claude-sonnet-4-6", 64_000},
		{"claude-sonnet-4-5", 64_000},
		{"openai-gpt-oss-120b", 16_384},
		{"openai-gpt-52", 65_536},
		{"openai-gpt-52-codex", 65_536},
		{"openai-gpt-53-codex", 128_000},
		{"openai-gpt-54", 131_072},
		{"openai-gpt-54-mini", 128_000},
		{"openai-gpt-54-pro", 128_000},
		{"openai-gpt-4o-2024-11-20", 16_384},
		{"openai-gpt-4o-mini-2024-07-18", 16_384},
		{"kimi-k2-thinking", 65_536},
		{"arcee-trinity-large-thinking", 65_536},
		{"kimi-k2-5", 65_536},
		{"deepseek-v3.2", 32_768},
		{"aion-labs-aion-2-0", 32_768},
		{"llama-3.2-3b", 4_096},
		{"llama-3.3-70b", 4_096},
		{"minimax-m25", 32_768},
		{"minimax-m27", 32_768},
		{"mercury-2", 50_000},
		{"qwen3-vl-235b-a22b", 16_384},
		{"nvidia-nemotron-3-nano-30b-a3b", 16_384},
		{"nvidia-nemotron-cascade-2-30b-a3b", 32_768},
	}
	for _, tt := range tests {
		entry, ok := findProviderModelEntry("venice", tt.model)
		if !ok {
			t.Fatalf("venice model %q missing from ProviderModels", tt.model)
		}
		if entry.OutputLimit != tt.wantOutput {
			t.Errorf("ProviderModels[venice][%q].OutputLimit = %d, want %d", tt.model, entry.OutputLimit, tt.wantOutput)
		}
	}
}

func TestProviderAliasResolvesLimits(t *testing.T) {
	// Simulate a custom provider "acme" that is type "venice"
	RegisterProviderAliases(map[string]string{"acme": "venice"})
	defer RegisterProviderAliases(nil)

	// Limits should resolve via "venice" explicit limits
	got := InputLimitForProviderModel("acme", "openai-gpt-54")
	if got != 1_000_000 {
		t.Errorf("InputLimitForProviderModel(acme, openai-gpt-54) = %d, want 1000000", got)
	}

	got = InputLimitForProviderModel("acme", "grok-41-fast")
	if got != 1_000_000 {
		t.Errorf("InputLimitForProviderModel(acme, grok-41-fast) = %d, want 1000000", got)
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

func TestDedupeEffortVariants(t *testing.T) {
	// claude-bin curated list has base + effort-suffixed aliases; the
	// suffixed ones should be dropped when the base is present.
	in := []string{
		"opus", "opus-low", "opus-medium", "opus-high", "opus-xhigh", "opus-max",
		"sonnet", "sonnet-low", "sonnet-medium", "sonnet-high",
		"haiku",
	}
	got := DedupeEffortVariants(in)
	want := []string{"opus", "sonnet", "haiku"}
	if len(got) != len(want) {
		t.Fatalf("DedupeEffortVariants: got %v, want %v", got, want)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("index %d: got %q, want %q", i, got[i], id)
		}
	}

	// Effort-suffixed entries without a base are preserved (e.g. some
	// provider might expose only the suffixed variant).
	in2 := []string{"gpt-5.4-high", "gpt-5.4-mini"}
	got2 := DedupeEffortVariants(in2)
	if len(got2) != 2 || got2[0] != "gpt-5.4-high" || got2[1] != "gpt-5.4-mini" {
		t.Errorf("expected both entries preserved, got %v", got2)
	}

	// Unrelated "-high"/"-low"/etc. suffixes without a base are preserved.
	in3 := []string{"claude-sonnet-4-6", "gpt-5.4"}
	got3 := DedupeEffortVariants(in3)
	if len(got3) != 2 {
		t.Errorf("expected both entries preserved, got %v", got3)
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

func findProviderModelEntry(provider, model string) (ModelEntry, bool) {
	for _, entry := range ProviderModels[provider] {
		if entry.ID == model {
			return entry, true
		}
	}
	return ModelEntry{}, false
}
