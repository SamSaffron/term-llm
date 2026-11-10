package llm

import (
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func allowedCatalogConfig(provider string, allowed []string) *config.Config {
	return &config.Config{Providers: map[string]config.ProviderConfig{provider: {AllowedModels: allowed}}}
}

func TestProviderModelIDsForDiscovery(t *testing.T) {
	cfg := allowedCatalogConfig("custom", []string{"allowed-base", "pinned-exact-high"})
	source := []string{"allowed-base", "allowed-base-high", "blocked-model", "pinned-exact-high-low"}
	got := ProviderModelIDsForDiscovery(cfg, "custom", source)
	for _, want := range []string{"allowed-base", "allowed-base-high", "pinned-exact-high"} {
		if !slices.Contains(got, want) {
			t.Errorf("ProviderModelIDsForDiscovery() = %v, want %q", got, want)
		}
	}
	for _, blocked := range []string{"blocked-model", "pinned-exact-high-low"} {
		if slices.Contains(got, blocked) {
			t.Errorf("ProviderModelIDsForDiscovery() exposed blocked %q: %v", blocked, got)
		}
	}
}

func TestExpandProviderModelIDsForDiscoveryKeepsBaseEfforts(t *testing.T) {
	RegisterConfigReasoningEfforts([]ConfigModelReasoningEfforts{{Provider: "custom", Model: "family", Efforts: []string{"low", "high"}}})
	t.Cleanup(func() { RegisterConfigReasoningEfforts(nil) })
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
		AllowedModels: []string{"family"},
		ModelConfigs: []config.ProviderModelConfig{{
			ID: "family", ReasoningEfforts: []string{"low", "high"},
		}},
	}}}
	got := ExpandProviderModelIDsForDiscovery(cfg, "custom", []string{"family", "other"})
	want := []string{"family", "family-low", "family-high"}
	if !slices.Equal(got, want) {
		t.Fatalf("ExpandProviderModelIDsForDiscovery() = %v, want %v", got, want)
	}
}

func TestExpandProviderModelIDsForDiscoveryExactEffortOnly(t *testing.T) {
	cfg := allowedCatalogConfig("custom", []string{"mystery-model-high"})
	got := ExpandProviderModelIDsForDiscovery(cfg, "custom", []string{"mystery-model"})
	if !slices.Equal(got, []string{"mystery-model-high"}) {
		t.Fatalf("ExpandProviderModelIDsForDiscovery() = %v, want the exact allowed effort", got)
	}
}

func TestExpandProviderModelIDsForDiscoveryAliasDoesNotDuplicate(t *testing.T) {
	RegisterConfigReasoningEfforts([]ConfigModelReasoningEfforts{
		{Provider: "custom", Model: "upstream/model", Efforts: []string{"low", "high"}},
		{Provider: "custom", Model: "friendly", Efforts: []string{"low", "high"}},
	})
	t.Cleanup(func() { RegisterConfigReasoningEfforts(nil) })
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
		AllowedModels: []string{"upstream/model-high"},
		ModelConfigs: []config.ProviderModelConfig{{
			ID: "upstream/model", Alias: "friendly", ReasoningEfforts: []string{"low", "high"},
		}},
	}}}
	got := ExpandProviderModelIDsForDiscovery(cfg, "custom", []string{"friendly"})
	if !slices.Equal(got, []string{"friendly-high"}) {
		t.Fatalf("ExpandProviderModelIDsForDiscovery() = %v, want only the alias effort", got)
	}
}

func TestProviderModelsForDiscoverySynthesizesAllowedID(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
		AllowedModels: []string{"pinned"},
		ModelConfigs: []config.ProviderModelConfig{{
			ID: "pinned", ReasoningEfforts: []string{"high"}, DefaultReasoningEffort: "high",
		}},
	}}}
	got := ProviderModelsForDiscovery(cfg, "custom", []core.ModelInfo{{ID: "blocked"}, {ID: "other"}})
	if len(got) != 1 || got[0].ID != "pinned" {
		t.Fatalf("ProviderModelsForDiscovery() = %+v, want pinned", got)
	}
	if !slices.Equal(got[0].ReasoningEfforts, []string{"high"}) || got[0].DefaultReasoningEffort != "high" {
		t.Fatalf("synthesized metadata = %+v", got[0])
	}
}

func TestProviderReasoningEffortsForDiscoveryFiltersVariants(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
		AllowedModels: []string{"upstream/model-high"},
		ModelConfigs: []config.ProviderModelConfig{{
			ID: "upstream/model", Alias: "friendly", ReasoningEfforts: []string{"low", "high"},
		}},
	}}}
	if got := ProviderReasoningEffortsForDiscovery(cfg, "custom", "friendly-high", []string{"low", "high"}); !slices.Equal(got, []string{"high"}) {
		t.Fatalf("ProviderReasoningEffortsForDiscovery() = %v, want [high]", got)
	}
}

func TestConfiguredProviderEffortProfileAllowed(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
		Model:         "friendly",
		AllowedModels: []string{"upstream/model-high"},
		ModelConfigs: []config.ProviderModelConfig{{
			ID: "upstream/model", Alias: "friendly", ReasoningEfforts: []string{"low", "high"},
		}},
	}}}
	if !configuredProviderEffortProfileAllowed(cfg, "custom", "high") {
		t.Fatal("allowed effort profile was hidden")
	}
	if configuredProviderEffortProfileAllowed(cfg, "custom", "low") {
		t.Fatal("blocked effort profile was offered")
	}
}

func TestGetProviderCompletionsOffersPinnedAllowedModelWithoutCatalog(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "custom",
		Providers: map[string]config.ProviderConfig{"custom": {
			Type:          config.ProviderTypeOpenAICompat,
			AllowedModels: []string{"pinned-model"},
		}},
	}
	got := GetProviderCompletions("custom:", false, cfg)
	if !slices.Contains(got, "custom:pinned-model") {
		t.Fatalf("GetProviderCompletions() = %v, want pinned allowlist entry", got)
	}
}

func TestImageCompletionsIgnoreTextAllowedModels(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"corp": {
		Type:          config.ProviderTypeOpenAICompat,
		AllowedModels: []string{"text-only"},
	}}}
	if got := GetProviderCompletions("corp:", true, cfg); len(got) != 0 {
		t.Fatalf("image completions = %v, want none from the text allowlist", got)
	}
}

func TestProviderModelDiscoveryAmazingModelIsNotAPrefix(t *testing.T) {
	source := []string{"amazing-model", "amazing-model-high", "amazing-model2", "amazing-model2-high", "amazing-model-mini"}
	cfg := &config.Config{
		DefaultProvider: "custom",
		Providers: map[string]config.ProviderConfig{"custom": {
			Type:          config.ProviderTypeOpenAICompat,
			Models:        source,
			AllowedModels: []string{"amazing-model"},
		}},
	}
	for _, tc := range []struct {
		name string
		got  []string
	}{
		{name: "listing", got: ProviderModelIDsForDiscovery(cfg, "custom", source)},
		{name: "expanded", got: ExpandProviderModelIDsForDiscovery(cfg, "custom", source)},
	} {
		if !slices.Contains(tc.got, "amazing-model") {
			t.Errorf("%s = %v, want amazing-model", tc.name, tc.got)
		}
		for _, blocked := range []string{"amazing-model2", "amazing-model2-high", "amazing-model-mini"} {
			if slices.Contains(tc.got, blocked) {
				t.Errorf("%s exposed prefix-neighbour %q: %v", tc.name, blocked, tc.got)
			}
		}
	}
	completions := GetProviderCompletions("custom:", false, cfg)
	for _, blocked := range []string{"custom:amazing-model2", "custom:amazing-model2-high", "custom:amazing-model-mini"} {
		if slices.Contains(completions, blocked) {
			t.Errorf("GetProviderCompletions() exposed %q: %v", blocked, completions)
		}
	}
	if !slices.Contains(completions, "custom:amazing-model") {
		t.Errorf("GetProviderCompletions() = %v, want amazing-model", completions)
	}
}

func TestGetProviderCompletionsRespectsProviderAllowedModels(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "custom",
		Providers: map[string]config.ProviderConfig{"custom": {
			Type:          config.ProviderTypeOpenAICompat,
			Models:        []string{"allowed", "blocked"},
			AllowedModels: []string{"allowed", "pinned"},
		}},
	}
	got := GetProviderCompletions("custom:", false, cfg)
	if !slices.Contains(got, "custom:allowed") || !slices.Contains(got, "custom:pinned") {
		t.Fatalf("GetProviderCompletions() = %v, want allowed and pinned", got)
	}
	if slices.Contains(got, "custom:blocked") {
		t.Fatalf("GetProviderCompletions() exposed blocked model: %v", got)
	}
}

func TestProviderModelsForDiscoveryRemovesBlockedDefaultEffort(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {AllowedModels: []string{"amazing-model-high"}}}}
	models := ProviderModelsForDiscovery(cfg, "custom", []core.ModelInfo{{ID: "amazing-model-high", ReasoningEfforts: []string{"low"}, DefaultReasoningEffort: "low"}})
	if len(models) != 1 || len(models[0].ReasoningEfforts) != 0 || models[0].DefaultReasoningEffort != "" {
		t.Fatalf("discovery advertised blocked effort metadata: %+v", models)
	}
}
