package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestIsModelOrEffortVariant(t *testing.T) {
	for _, tc := range []struct {
		provider, base, selected string
		want                     bool
	}{
		{"chatgpt", "gpt-6-sol", "gpt-6-sol", true},
		{"chatgpt", "gpt-6-sol", "gpt-6-sol-high", true},
		{"chatgpt", "gpt-6-sol", "gpt-6-sol-max", true},
		{"chatgpt", "gpt-6-sol", "gpt-6-sol-mini", false},
		{"chatgpt", "gpt-5.1-codex", "gpt-5.1-codex-max", false},
		{"chatgpt", "gpt-5.3-codex", "gpt-5.3-codex-spark", false},
		{"claude-bin", "opus", "opus-max", true},
		{"agy-bin", "gemini-3.6-flash", "gemini-3.6-flash-high", true},
		{"cursor-bin", "literal-model", "literal-model-high", true},
		{"chatgpt", "gpt-6-sol-high", "gpt-6-sol-high-max", false},
	} {
		t.Run(tc.provider+"/"+tc.base+"/"+tc.selected, func(t *testing.T) {
			if got := IsModelOrEffortVariant(tc.provider, tc.selected, tc.base); got != tc.want {
				t.Errorf("match = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestConfigDeclaredModelDoesNotUseUnknownEffortHeuristic(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {Models: []string{"gpt-6-sol"}, ModelConfigs: []config.ProviderModelConfig{{ID: "gpt-6-sol"}}}}}
	if IsModelOrEffortVariantForConfig(cfg, "custom", "gpt-6-sol-high", "gpt-6-sol") {
		t.Fatal("declared literal model falsely matched heuristic suffix")
	}
	cfg.Providers["custom"] = config.ProviderConfig{Models: []string{"gpt-6-sol"}, ModelConfigs: []config.ProviderModelConfig{{ID: "gpt-6-sol", ReasoningEfforts: []string{"high"}}}}
	if !IsModelOrEffortVariantForConfig(cfg, "custom", "gpt-6-sol-high", "gpt-6-sol") {
		t.Fatal("declared supported effort rejected")
	}
}

func TestResolveFastTargetHonorsTargetProviderType(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"primary": {FastProvider: "custom"},
		"custom":  {Type: config.ProviderTypeChatGPT},
	}}
	key, model, ok := ResolveFastTarget(cfg, "primary")
	if !ok || key != "custom" || model != ProviderFastModels["chatgpt"] {
		t.Fatalf("target=%s:%s ok=%v", key, model, ok)
	}
}

func TestKnownModelIDEndingInEffortSuffixCanExposeEfforts(t *testing.T) {
	const provider = "policy-known-suffixed"
	ProviderModels[provider] = []ModelEntry{{ID: "literal-high", ReasoningEfforts: []string{"max"}}}
	t.Cleanup(func() { delete(ProviderModels, provider) })
	if !IsModelOrEffortVariant(provider, "literal-high-max", "literal-high") {
		t.Fatal("catalog model with effort-like ID should match its declared max effort")
	}
	if IsModelOrEffortVariant(provider, "future-high-max", "future-high") {
		t.Fatal("unknown effort-suffixed rule should remain exact-only")
	}
}
