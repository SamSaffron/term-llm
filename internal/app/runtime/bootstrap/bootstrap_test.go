package bootstrap

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"testing"
)

func TestApplyProviderOverridesWithAgentResolvesFastModelAlias(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"openai": {Type: config.ProviderTypeOpenAI},
		},
	}

	if err := ApplyProviderOverridesWithAgent(cfg, "", "", "", "", "fast"); err != nil {
		t.Fatalf("applyProviderOverridesWithAgent: %v", err)
	}

	if cfg.DefaultProvider != "openai" {
		t.Fatalf("DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}
	if got := cfg.Providers["openai"].Model; got != "gpt-5.6-luna" {
		t.Fatalf("openai model = %q, want gpt-5.6-luna", got)
	}
}

func TestApplyProviderOverridesWithAgentResolvesFastProvider(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "workhorse",
		Providers: map[string]config.ProviderConfig{
			"workhorse": {
				Type:         config.ProviderTypeAnthropic,
				FastProvider: "cheap-openai",
				FastModel:    "gpt-fast-custom",
			},
			"cheap-openai": {Type: config.ProviderTypeOpenAI},
		},
	}

	if err := ApplyProviderOverridesWithAgent(cfg, "", "", "", "", "fast"); err != nil {
		t.Fatalf("applyProviderOverridesWithAgent: %v", err)
	}

	if cfg.DefaultProvider != "cheap-openai" {
		t.Fatalf("DefaultProvider = %q, want cheap-openai", cfg.DefaultProvider)
	}
	if got := cfg.Providers["cheap-openai"].Model; got != "gpt-fast-custom" {
		t.Fatalf("cheap-openai model = %q, want gpt-fast-custom", got)
	}
}

func TestApplyProviderOverridesWithAgentCLISkipsAgentFastModel(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"openai": {Type: config.ProviderTypeOpenAI},
		},
	}

	if err := ApplyProviderOverridesWithAgent(cfg, "", "", "anthropic:claude-sonnet-4-6", "", "fast"); err != nil {
		t.Fatalf("applyProviderOverridesWithAgent: %v", err)
	}

	if cfg.DefaultProvider != "anthropic" {
		t.Fatalf("DefaultProvider = %q, want anthropic", cfg.DefaultProvider)
	}
	if got := cfg.Providers["anthropic"].Model; got != "claude-sonnet-4-6" {
		t.Fatalf("anthropic model = %q, want claude-sonnet-4-6", got)
	}
	if got := cfg.Providers["openai"].Model; got != "" {
		t.Fatalf("openai model = %q, want untouched empty model", got)
	}
}

func TestRegisterModelLimitsRegistersVLLMReasoningEnabled(t *testing.T) {
	defer llm.RegisterConfigReasoningEfforts(nil)
	defer llm.RegisterConfigLimits(nil)
	defer llm.RegisterProviderAliases(nil)

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"cdck_qwen": {
			Type:            config.ProviderTypeVLLM,
			Model:           "Qwen/Qwen3.5-122B-A10B",
			Models:          []string{"Qwen/Qwen3.5-122B-A10B-Instruct"},
			ModelConfigs:    []config.ProviderModelConfig{{ID: "Qwen/Qwen3.5-122B-A10B", Alias: "qwen", ReasoningEfforts: []string{"none", "low", "high"}, DefaultReasoningEffort: "high"}},
			Reasoning:       "enabled",
			ContextWindow:   200000,
			MaxOutputTokens: 50000,
		},
	}}

	RegisterModelLimits(cfg)

	explicitWant := []string{"none", "low", "high"}
	for _, model := range []string{"Qwen/Qwen3.5-122B-A10B", "qwen", "qwen-high"} {
		got := llm.ReasoningEffortsForProviderModel("cdck_qwen", model)
		if !equalStringSlices(got, explicitWant) {
			t.Fatalf("explicit reasoning efforts for %s = %v, want %v", model, got, explicitWant)
		}
	}
	genericWant := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	if got := llm.ReasoningEffortsForProviderModel("cdck_qwen", "Qwen/Qwen3.5-122B-A10B-Instruct"); !equalStringSlices(got, genericWant) {
		t.Fatalf("generic reasoning efforts = %v, want %v", got, genericWant)
	}
	base, effort := llm.BaseModelAndEffortForProvider("cdck_qwen", "Qwen/Qwen3.5-122B-A10B-high")
	if base != "Qwen/Qwen3.5-122B-A10B" || effort != "high" {
		t.Fatalf("BaseModelAndEffortForProvider = (%q, %q), want qwen/high", base, effort)
	}
	if got := llm.InputLimitForProviderModel("cdck_qwen", "Qwen/Qwen3.5-122B-A10B-high"); got != 150000 {
		t.Fatalf("InputLimitForProviderModel suffixed = %d, want 150000", got)
	}
}

func equalStringSlices(a, b []string) bool {
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

func TestRegisterChatGPTContextOverrides(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	defer RegisterModelLimits(&config.Config{})
	RegisterModelLimits(&config.Config{Providers: map[string]config.ProviderConfig{
		"work": {Type: config.ProviderTypeChatGPT, ContextWindow: 500000, ModelConfigs: []config.ProviderModelConfig{{ID: "gpt-6-astra", Alias: "astra", ContextWindow: 600000}}},
	}})
	for _, tc := range []struct {
		provider, model string
		want            int
	}{
		{"work", "astra-high", 600000}, {"work", "gpt-6-astra-high", 600000},
		{"work", "gpt-5.6-sol", 500000}, {"chatgpt", "gpt-6-astra", 372000},
	} {
		if got := llm.InputLimitForProviderModel(tc.provider, tc.model); got != tc.want {
			t.Fatalf("%s:%s got %d want %d", tc.provider, tc.model, got, tc.want)
		}
	}
}
