package llm

import "testing"

func TestProviderModelsIncludeGrokBin(t *testing.T) {
	ids := ProviderModelIDs("grok-bin")
	for _, want := range []string{"grok-4.5", "grok-4.5-low", "grok-4.5-medium", "grok-4.5-high", "grok-4.5-xhigh", "grok-composer-2.5-fast"} {
		if !containsModelID(ids, want) {
			t.Fatalf("grok-bin models missing %q: %v", want, ids)
		}
	}
	if !containsModelID(GetBuiltInProviderNames(), "grok-bin") {
		t.Fatal("GetBuiltInProviderNames missing grok-bin")
	}
	wantEfforts := []string{"low", "medium", "high", "xhigh"}
	if got := ReasoningEffortsForProviderModel("grok-bin", "grok-4.5-high"); !equalSlice(got, wantEfforts) {
		t.Fatalf("grok-bin efforts = %v, want %v", got, wantEfforts)
	}
	base, effort := BaseModelAndEffortForProvider("grok-bin", "grok-4.5-xhigh")
	if base != "grok-4.5" || effort != "xhigh" {
		t.Fatalf("grok-bin base/effort = (%q, %q)", base, effort)
	}
}

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

func TestCopilotProviderModelsAreDynamicCacheOnly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if _, ok := ProviderModels["copilot"]; ok {
		t.Fatal("ProviderModels should not contain a hardcoded Copilot model list")
	}
	if ids := ProviderModelIDs("copilot"); len(ids) != 0 {
		t.Fatalf("ProviderModelIDs(copilot) before cache = %v, want empty", ids)
	}

	if got := InputLimitForProviderModel("copilot", "gpt-5.5"); got != 0 {
		t.Fatalf("InputLimitForProviderModel(copilot, gpt-5.5) before cache = %d, want 0", got)
	}

	RefreshCopilotCacheSync([]ModelInfo{
		{ID: "cached-copilot-a", InputLimit: 123_456},
		{ID: "cached-copilot-b"},
	})
	ids := ProviderModelIDs("copilot")
	want := []string{"cached-copilot-a", "cached-copilot-b"}
	if !equalSlice(ids, want) {
		t.Fatalf("ProviderModelIDs(copilot) = %v, want %v", ids, want)
	}
	if got := InputLimitForProviderModel("copilot", "cached-copilot-a"); got != 123_456 {
		t.Fatalf("InputLimitForProviderModel(copilot, cached-copilot-a) = %d, want 123456", got)
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
		"fable": true, "fable-low": true, "fable-medium": true, "fable-high": true, "fable-xhigh": true, "fable-max": true,
		"haiku": true,
		// grok-bin CLI aliases have subscription-dependent context limits.
		"grok-4.5": true, "grok-4.5-low": true, "grok-4.5-medium": true, "grok-4.5-high": true, "grok-4.5-xhigh": true,
		"grok-composer-2.5-fast": true,
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

func TestProviderAliasResolvesConfigLimits(t *testing.T) {
	// Simulate a custom provider "acme" that is type "venice". Venice no longer
	// has curated static model limits; aliases should still preserve explicit
	// user/config limits.
	RegisterProviderAliases(map[string]string{"acme": "venice"})
	defer RegisterProviderAliases(nil)
	RegisterConfigLimits([]ConfigModelLimit{{Provider: "acme", Model: "custom-venice-model", InputLimit: 123_000}})
	defer RegisterConfigLimits(nil)

	got := InputLimitForProviderModel("acme", "custom-venice-model")
	if got != 123_000 {
		t.Errorf("InputLimitForProviderModel(acme, custom-venice-model) = %d, want 123000", got)
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
		{"claude-bin", "fable", []string{"low", "medium", "high", "xhigh", "max"}},
		{"claude-bin", "fable-high", []string{"low", "medium", "high", "xhigh", "max"}},
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
		{"anthropic", "claude-fable-5", []string{"low", "medium", "high", "xhigh", "max"}},
		{"anthropic", "claude-fable-5-max", []string{"low", "medium", "high", "xhigh", "max"}},
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
		{"claude-bin", "fable-max", "fable", "max"},
		{"anthropic", "claude-opus-4-8-max", "claude-opus-4-8", "max"},
		{"anthropic", "claude-fable-5-max", "claude-fable-5", "max"},
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
	got := ExpandWithEffortVariantsForProvider("claude-bin", []string{"opus", "sonnet", "fable", "haiku"})
	for _, want := range []string{"opus-low", "opus-medium", "opus-high", "opus-xhigh", "opus-max", "sonnet-low", "sonnet-medium", "sonnet-high", "fable-low", "fable-medium", "fable-high", "fable-xhigh", "fable-max"} {
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
