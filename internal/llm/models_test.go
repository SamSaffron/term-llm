package llm

import "testing"

func TestProviderModelsIncludeLatestOpenAIModels(t *testing.T) {
	if !containsModelID(ProviderModelIDs("openai"), "gpt-5.5") {
		t.Fatalf("openai models missing gpt-5.5")
	}
	if got := InputLimitForProviderModel("openai", "gpt-5.5"); got != 922_000 {
		t.Fatalf("openai gpt-5.5 input limit = %d, want %d", got, 922_000)
	}
	if got := OutputLimitForModel("gpt-5.5"); got != 128_000 {
		t.Fatalf("gpt-5.5 output limit = %d, want %d", got, 128_000)
	}
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

func TestProviderModelsIncludeNearAICloudDefaults(t *testing.T) {
	if !containsModelID(ProviderModelIDs("nearai"), "zai-org/GLM-5.1-FP8") {
		t.Fatalf("nearai models missing zai-org/GLM-5.1-FP8")
	}
	if got := ProviderFastModels["nearai"]; got != "Qwen/Qwen3.6-35B-A3B-FP8" {
		t.Fatalf("ProviderFastModels[nearai] = %q, want Qwen/Qwen3.6-35B-A3B-FP8", got)
	}
	if input, output, ok := PricingForProviderModel("nearai", "zai-org/GLM-5.1-FP8"); !ok || input != 0.85 || output != 3.30 {
		t.Fatalf("NEAR AI GLM pricing = %g/%g ok=%t, want 0.85/3.30 true", input, output, ok)
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
		{"kimi-k2-6", 256_000},
		{"kimi-k2-thinking", 256_000},
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
		{"kimi-k2-6", 65_536},
		{"deepseek-v3.2", 32_768},
		// E2EE variants
		{"e2ee-gemma-3-27b-p", 16_384},
		{"e2ee-glm-4-7-flash-p", 24_000},
		{"e2ee-glm-4-7-p", 16_384},
		{"e2ee-glm-5", 32_000},
		{"e2ee-gpt-oss-120b-p", 16_384},
		{"e2ee-gpt-oss-20b-p", 16_384},
		{"e2ee-qwen-2-5-7b-p", 8_192},
		{"e2ee-qwen3-30b-a3b-p", 16_384},
		{"e2ee-qwen3-5-122b-a10b", 32_768},
		{"e2ee-qwen3-vl-30b-a3b-p", 16_384},
		{"e2ee-venice-uncensored-24b-p", 8_192},
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

func TestReasoningEffortsForProviderModel(t *testing.T) {
	tests := []struct {
		provider string
		model    string
		want     []string
	}{
		{"claude-bin", "opus", []string{"low", "medium", "high", "xhigh", "max"}},
		{"claude-bin", "opus-high", []string{"low", "medium", "high", "xhigh", "max"}},
		{"claude-bin", "sonnet", []string{"low", "medium", "high"}},
		{"claude-bin", "sonnet-high", []string{"low", "medium", "high"}},
		{"claude-bin", "haiku", nil},
		{"openai", "gpt-5.4", []string{"minimal", "low", "medium", "high", "xhigh"}},
		{"openai", "gpt-5.4-high", []string{"minimal", "low", "medium", "high", "xhigh"}},
		{"openai", "gpt-5.6", []string{"minimal", "low", "medium", "high", "xhigh"}},
		{"anthropic", "claude-opus-4-8", []string{"low", "medium", "high", "xhigh", "max"}},
		{"anthropic", "claude-opus-4-8-max", []string{"low", "medium", "high", "xhigh", "max"}},
		{"anthropic", "claude-sonnet-4-6", []string{"low", "medium", "high"}},
		{"anthropic", "claude-sonnet-4-6-high", []string{"low", "medium", "high"}},
		{"anthropic", "claude-haiku-4-5", nil},
		{"anthropic", "claude-sonnet-4-5-1m", []string{"low", "medium", "high"}},
	}

	for _, tt := range tests {
		t.Run(tt.provider+":"+tt.model, func(t *testing.T) {
			got := ReasoningEffortsForProviderModel(tt.provider, tt.model)
			if !equalSlice(got, tt.want) {
				t.Fatalf("ReasoningEffortsForProviderModel(%q, %q) = %v, want %v", tt.provider, tt.model, got, tt.want)
			}
		})
	}
}

func TestReasoningEffortsAreProviderSpecific(t *testing.T) {
	if got := ReasoningEffortsForProviderModel("openai", "gpt-5.4"); containsModelID(got, "max") {
		t.Fatalf("openai:gpt-5.4 efforts unexpectedly include max: %v", got)
	}
	if got := ReasoningEffortsForProviderModel("claude-bin", "sonnet"); containsModelID(got, "max") || containsModelID(got, "xhigh") {
		t.Fatalf("claude-bin:sonnet efforts unexpectedly include opus-only levels: %v", got)
	}
}

func TestBaseModelAndEffortForProviderAvoidsFalseMaxParsing(t *testing.T) {
	tests := []struct {
		provider   string
		model      string
		wantBase   string
		wantEffort string
	}{
		{"openai", "gpt-5.4-high", "gpt-5.4", "high"},
		{"openai", "gpt-5.4-max", "gpt-5.4-max", ""},
		{"chatgpt", "gpt-5.1-codex-max", "gpt-5.1-codex-max", ""},
		{"chatgpt", "gpt-5.1-codex-high", "gpt-5.1-codex", "high"},
		{"claude-bin", "opus-max", "opus", "max"},
		{"claude-bin", "sonnet-max", "sonnet-max", ""},
		{"anthropic", "claude-opus-4-8-max", "claude-opus-4-8", "max"},
		{"anthropic", "claude-sonnet-4-6-high", "claude-sonnet-4-6", "high"},
		{"anthropic", "claude-sonnet-4-6-max", "claude-sonnet-4-6-max", ""},
		{"anthropic", "claude-haiku-4-5-high", "claude-haiku-4-5-high", ""},
	}

	for _, tt := range tests {
		t.Run(tt.provider+":"+tt.model, func(t *testing.T) {
			gotBase, gotEffort := BaseModelAndEffortForProvider(tt.provider, tt.model)
			if gotBase != tt.wantBase || gotEffort != tt.wantEffort {
				t.Fatalf("BaseModelAndEffortForProvider(%q, %q) = (%q, %q), want (%q, %q)", tt.provider, tt.model, gotBase, gotEffort, tt.wantBase, tt.wantEffort)
			}
		})
	}
}

func TestExpandWithEffortVariantsForProvider(t *testing.T) {
	got := ExpandWithEffortVariantsForProvider("claude-bin", []string{"opus", "sonnet", "haiku"})
	for _, want := range []string{"opus-low", "opus-medium", "opus-high", "opus-xhigh", "opus-max", "sonnet-low", "sonnet-medium", "sonnet-high"} {
		if !containsModelID(got, want) {
			t.Fatalf("expanded claude-bin models missing %q: %v", want, got)
		}
	}
	if containsModelID(got, "sonnet-max") || containsModelID(got, "sonnet-xhigh") || containsModelID(got, "haiku-medium") {
		t.Fatalf("expanded claude-bin models included invalid variants: %v", got)
	}

	got = ExpandWithEffortVariantsForProvider("openai", []string{"gpt-5.4", "gpt-5.4-high", "gpt-5.1-codex-max"})
	for _, want := range []string{"gpt-5.4-minimal", "gpt-5.4-low", "gpt-5.4-medium", "gpt-5.4-high", "gpt-5.4-xhigh"} {
		if !containsModelID(got, want) {
			t.Fatalf("expanded openai models missing %q: %v", want, got)
		}
	}
	if containsModelID(got, "gpt-5.4-max") || containsModelID(got, "gpt-5.1-codex-max-high") {
		t.Fatalf("expanded openai models included invalid max-derived variants: %v", got)
	}

	got = ExpandWithEffortVariantsForProvider("anthropic", []string{"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5"})
	for _, want := range []string{"claude-opus-4-8-low", "claude-opus-4-8-max", "claude-sonnet-4-6-low", "claude-sonnet-4-6-high"} {
		if !containsModelID(got, want) {
			t.Fatalf("expanded anthropic models missing %q: %v", want, got)
		}
	}
	if containsModelID(got, "claude-sonnet-4-6-max") || containsModelID(got, "claude-haiku-4-5-low") {
		t.Fatalf("expanded anthropic models included invalid variants: %v", got)
	}
}

func TestProviderAliasResolvesReasoningEfforts(t *testing.T) {
	RegisterProviderAliases(map[string]string{"my-openai": "openai"})
	defer RegisterProviderAliases(nil)

	got := ReasoningEffortsForProviderModel("my-openai", "gpt-5.4-high")
	want := []string{"minimal", "low", "medium", "high", "xhigh"}
	if !equalSlice(got, want) {
		t.Fatalf("alias efforts = %v, want %v", got, want)
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

func TestAnthropicProviderModelsIncludeLatestOpus(t *testing.T) {
	entry, ok := findProviderModelEntry("anthropic", "claude-opus-4-8")
	if !ok {
		t.Fatal("ProviderModels[anthropic] missing claude-opus-4-8")
	}
	if entry.InputLimit != 980_000 || entry.OutputLimit != 128_000 {
		t.Fatalf("claude-opus-4-8 limits = input %d output %d, want 980000/128000", entry.InputLimit, entry.OutputLimit)
	}
	if got := ReasoningEffortsForProviderModel("anthropic", "claude-opus-4-8"); !equalSlice(got, claudeBinOpusEffortVariants) {
		t.Fatalf("claude-opus-4-8 reasoning efforts = %v, want %v", got, claudeBinOpusEffortVariants)
	}
}

func TestConfigReasoningEffortsForVLLMCustomProvider(t *testing.T) {
	RegisterConfigReasoningEfforts([]ConfigModelReasoningEfforts{{
		Provider: "cdck_qwen",
		Model:    "Qwen/Qwen3.5-122B-A10B",
		Efforts:  DefaultReasoningEffortsForProviderType("vllm"),
	}})
	defer RegisterConfigReasoningEfforts(nil)

	want := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	got := ReasoningEffortsForProviderModel("cdck_qwen", "Qwen/Qwen3.5-122B-A10B")
	if !equalSlice(got, want) {
		t.Fatalf("ReasoningEffortsForProviderModel custom vllm = %v, want %v", got, want)
	}

	base, effort := BaseModelAndEffortForProvider("cdck_qwen", "Qwen/Qwen3.5-122B-A10B-medium")
	if base != "Qwen/Qwen3.5-122B-A10B" || effort != "medium" {
		t.Fatalf("BaseModelAndEffortForProvider custom vllm = (%q, %q), want base + medium", base, effort)
	}

	expanded := ExpandWithEffortVariantsForProvider("cdck_qwen", []string{"Qwen/Qwen3.5-122B-A10B"})
	for _, wantModel := range []string{"Qwen/Qwen3.5-122B-A10B-minimal", "Qwen/Qwen3.5-122B-A10B-medium", "Qwen/Qwen3.5-122B-A10B-max"} {
		if !containsModelID(expanded, wantModel) {
			t.Fatalf("expanded custom vllm models missing %q: %v", wantModel, expanded)
		}
	}
}

func TestConfigReasoningEffortsClearedOnReload(t *testing.T) {
	RegisterConfigReasoningEfforts([]ConfigModelReasoningEfforts{{Provider: "cdck_qwen", Model: "qwen", Efforts: []string{"low"}}})
	RegisterConfigReasoningEfforts(nil)

	if got := ReasoningEffortsForProviderModel("cdck_qwen", "qwen"); len(got) != 0 {
		t.Fatalf("expected cleared config reasoning efforts, got %v", got)
	}
}

func TestDedupeEffortVariantsForProviderAvoidsFalseMaxDrops(t *testing.T) {
	got := DedupeEffortVariantsForProvider("chatgpt", []string{"gpt-5.1-codex", "gpt-5.1-codex-max", "gpt-5.1-codex-high"})
	want := []string{"gpt-5.1-codex", "gpt-5.1-codex-max"}
	if !equalSlice(got, want) {
		t.Fatalf("DedupeEffortVariantsForProvider(chatgpt) = %v, want %v", got, want)
	}

	got = DedupeEffortVariantsForProvider("claude-bin", []string{"opus", "opus-max", "sonnet", "sonnet-high", "haiku"})
	want = []string{"opus", "sonnet", "haiku"}
	if !equalSlice(got, want) {
		t.Fatalf("DedupeEffortVariantsForProvider(claude-bin) = %v, want %v", got, want)
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

func TestSortModelIDsByPopularity(t *testing.T) {
	t.Run("respects curated order regardless of input order", func(t *testing.T) {
		// openai curated list is gpt-5.4 first, then gpt-5.4-mini, etc.
		// Feed in reverse-alpha to prove curated rank wins over input order.
		in := []string{"o3-mini", "gpt-5", "gpt-5.4-mini", "gpt-5.4"}
		got := SortModelIDsByPopularity("openai", "", in)
		want := []string{"gpt-5.4", "gpt-5.4-mini", "gpt-5", "o3-mini"}
		if !equalSlice(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("pins defaultModel even when not in curated list", func(t *testing.T) {
		// User configured a custom fine-tune; it should still appear first
		// so the picker stays usable when the upstream API drops it.
		got := SortModelIDsByPopularity("openai", "my-finetune", []string{"gpt-5.4", "gpt-5"})
		if len(got) < 1 || got[0] != "my-finetune" {
			t.Errorf("expected configured default at position 0, got %v", got)
		}
		// The rest should be in curated order.
		if got[1] != "gpt-5.4" || got[2] != "gpt-5" {
			t.Errorf("expected curated order after pinned default, got %v", got)
		}
	})

	t.Run("dedupes when defaultModel is also in ids", func(t *testing.T) {
		got := SortModelIDsByPopularity("openai", "gpt-5.4", []string{"gpt-5.4", "gpt-5", "gpt-5.4"})
		count := 0
		for _, id := range got {
			if id == "gpt-5.4" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected gpt-5.4 to appear exactly once, got %d in %v", count, got)
		}
		if got[0] != "gpt-5.4" {
			t.Errorf("expected pinned default first, got %v", got)
		}
	})

	t.Run("alpha-sorts unknown models after curated", func(t *testing.T) {
		// "zzz-future" and "aaa-future" are not in the openai curated list,
		// so they should appear after every curated hit, alpha-sorted.
		got := SortModelIDsByPopularity("openai", "", []string{"zzz-future", "gpt-5.4", "aaa-future"})
		if got[0] != "gpt-5.4" {
			t.Errorf("expected curated model first, got %v", got)
		}
		// aaa-future must precede zzz-future at the tail.
		var aaaIdx, zzzIdx int
		for i, id := range got {
			if id == "aaa-future" {
				aaaIdx = i
			}
			if id == "zzz-future" {
				zzzIdx = i
			}
		}
		if aaaIdx >= zzzIdx {
			t.Errorf("expected alpha order for unknowns: aaa before zzz, got %v", got)
		}
	})

	t.Run("resolves provider aliases for ranking", func(t *testing.T) {
		// Map a custom name "acme" → built-in "venice" so the venice curated
		// ranking applies to alias inputs.
		RegisterProviderAliases(map[string]string{"acme": "venice"})
		defer RegisterProviderAliases(nil)

		veniceIDs := ResolveProviderModelIDs("acme")
		if len(veniceIDs) < 2 {
			t.Skip("venice curated list too small for this test")
		}
		// Reverse the curated list and confirm it gets re-ordered correctly.
		reversed := make([]string, len(veniceIDs))
		for i, id := range veniceIDs {
			reversed[len(veniceIDs)-1-i] = id
		}
		got := SortModelIDsByPopularity("acme", "", reversed)
		if !equalSlice(got, veniceIDs) {
			t.Errorf("alias-resolved ranking failed:\n got=%v\nwant=%v", got, veniceIDs)
		}
	})
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
