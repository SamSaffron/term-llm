package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/anthropic"
)

// anthropicModelCacheKey is the cache key of the public-API catalog.
const anthropicModelCacheKey = "anthropic"

// The curated fallback must mirror the recorded live catalog so completion is
// correct before the first refresh.
func TestAnthropicStaticModelsMatchRecordedCatalog(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(anthropicGoldenDir, "list_models", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var live []core.ModelInfo
	if err := json.Unmarshal(data, &live); err != nil {
		t.Fatal(err)
	}
	static := ProviderModels["anthropic"]
	if len(static) != len(live) {
		t.Fatalf("static list has %d models, recorded catalog has %d", len(static), len(live))
	}
	for i, model := range live {
		entry := static[i]
		if entry.ID != model.ID || entry.InputLimit != model.InputLimit || entry.OutputLimit != model.OutputLimit || !equalSlice(entry.ReasoningEfforts, model.ReasoningEfforts) {
			t.Errorf("static[%d] = %+v, recorded = {ID:%s InputLimit:%d OutputLimit:%d Efforts:%v}", i, entry, model.ID, model.InputLimit, model.OutputLimit, model.ReasoningEfforts)
		}
	}
}

func withAnthropicCache(t *testing.T, models []core.ModelInfo) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if models != nil {
		cached := make([]cache.CachedModel, 0, len(models))
		for _, m := range models {
			cached = append(cached, cache.CachedModel{
				ID: m.ID, DisplayName: m.DisplayName, Created: m.Created, OwnedBy: m.OwnedBy,
				InputLimit: m.InputLimit, OutputLimit: m.OutputLimit, ConfiguredContext: m.ConfiguredContext,
				InputPrice: m.InputPrice, OutputPrice: m.OutputPrice,
				ReasoningEfforts: m.ReasoningEfforts, ReasoningModes: m.ReasoningModes,
			})
		}
		if err := cache.WriteModelInfoCache(anthropicModelCacheKey, cached); err != nil {
			t.Fatal(err)
		}
	}
}

// isAdaptiveModel reports whether a direct Anthropic model uses adaptive
// thinking, via the same decoding the provider applies to "-thinking" models.
func isAdaptiveModel(model string) bool {
	return anthropic.NewTransportProvider(model+"-thinking", nil, nil, nil).Details().Adaptive
}

func TestAnthropicLiveCatalogTakesPrecedence(t *testing.T) {
	withAnthropicCache(t, []core.ModelInfo{
		{ID: "claude-next-1", InputLimit: 480_000, OutputLimit: 96_000, ReasoningEfforts: []string{"low", "high"}, ReasoningModes: []string{"adaptive"}},
		{ID: "claude-old-1-20240101", ReasoningModes: []string{"enabled"}},
	})

	if got := ProviderModelIDs("anthropic"); !equalSlice(got, []string{"claude-next-1", "claude-old-1-20240101"}) {
		t.Fatalf("ProviderModelIDs = %v, want cached catalog", got)
	}
	if got := ReasoningEffortsForProviderModel("anthropic", "claude-next-1"); !equalSlice(got, []string{"low", "high"}) {
		t.Fatalf("efforts = %v", got)
	}
	if base, effort := BaseModelAndEffortForProvider("anthropic", "claude-next-1-high"); base != "claude-next-1" || effort != "high" {
		t.Fatalf("BaseModelAndEffortForProvider = %q, %q", base, effort)
	}
	if got := InputLimitForProviderModel("anthropic", "claude-next-1"); got != 480_000 {
		t.Fatalf("input limit = %d", got)
	}
	if !isAdaptiveModel("claude-next-1") {
		t.Fatal("cached adaptive model treated as budget-only")
	}
	// Undated aliases resolve to their dated snapshot.
	if isAdaptiveModel("claude-old-1") {
		t.Fatal("cached budget-only model treated as adaptive")
	}
}

func TestAnthropicFallbackWithoutCache(t *testing.T) {
	withAnthropicCache(t, nil)
	if got := ProviderModelIDs("anthropic"); len(got) == 0 || got[0] != "claude-haiku-5-5" {
		t.Fatalf("fallback IDs = %v", got)
	}
	for model, want := range map[string]bool{
		"claude-opus-5": true, "claude-sonnet-4-6": true, "claude-opus-6": true,
		"claude-haiku-4-5": false, "claude-opus-4-5-20251101": false, "claude-sonnet-4": false,
	} {
		if got := isAdaptiveModel(model); got != want {
			t.Errorf("isAdaptiveModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestAnthropicOneMillionKeepsBudgetWithWarmCache(t *testing.T) {
	withAnthropicCache(t, []core.ModelInfo{{ID: "claude-sonnet-4-5-20250929", InputLimit: 180_000, ReasoningModes: []string{"enabled"}}})
	if got := InputLimitForProviderModel("anthropic", "claude-sonnet-4-5"); got != 180_000 {
		t.Fatalf("base model limit = %d, want cached 180000", got)
	}
	for _, model := range []string{"claude-sonnet-4-5-1m", "claude-sonnet-4-5-20250929-1m"} {
		if got := InputLimitForProviderModel("anthropic", model); got != 980_000 {
			t.Errorf("InputLimitForProviderModel(%q) = %d, want 980000", model, got)
		}
	}
}

// Curated rows mirror the API but must not shadow a newer live catalog.
func TestAnthropicLiveCatalogOverridesCuratedRows(t *testing.T) {
	withAnthropicCache(t, []core.ModelInfo{{ID: "claude-sonnet-4-6", InputLimit: 400_000, OutputLimit: 32_000, ReasoningEfforts: []string{"low", "xhigh"}, ReasoningModes: []string{"adaptive"}}})
	if got := InputLimitForProviderModel("anthropic", "claude-sonnet-4-6"); got != 400_000 {
		t.Errorf("input limit = %d, want cached 400000", got)
	}
	if got := InputLimitForProviderModel("anthropic", "claude-sonnet-4-6-xhigh"); got != 400_000 {
		t.Errorf("effort-suffixed input limit = %d, want cached 400000", got)
	}
	if got := ReasoningEffortsForProviderModel("anthropic", "claude-sonnet-4-6"); !equalSlice(got, []string{"low", "xhigh"}) {
		t.Errorf("efforts = %v, want cached", got)
	}
	if base, effort := BaseModelAndEffortForProvider("anthropic", "claude-sonnet-4-6-xhigh"); base != "claude-sonnet-4-6" || effort != "xhigh" {
		t.Errorf("BaseModelAndEffortForProvider = %q, %q", base, effort)
	}
	if got := anthropic.ClampOutputTokensForRequest(100_000, "claude-sonnet-4-6"); got != 32_000 {
		t.Errorf("output clamp = %d, want cached 32000", got)
	}
}

func TestAnthropicOneMillionWithEffortSuffix(t *testing.T) {
	withAnthropicCache(t, []core.ModelInfo{{ID: "claude-sonnet-4-5-20250929", InputLimit: 180_000}})
	if got := InputLimitForProviderModel("anthropic", "claude-sonnet-4-5-1m-high"); got != 980_000 {
		t.Fatalf("-1m with effort suffix = %d, want 980000", got)
	}
}

func TestAnthropicCachedIDsOnlyForBuiltInProvider(t *testing.T) {
	withAnthropicCache(t, []core.ModelInfo{{ID: "claude-live-only"}})
	RegisterProviderAliases(map[string]string{"my-gateway": "anthropic"})
	defer RegisterProviderAliases(nil)
	if got := ProviderModelIDs("anthropic"); !equalSlice(got, []string{"claude-live-only"}) {
		t.Fatalf("built-in anthropic IDs = %v", got)
	}
	if got := ProviderModelIDs("my-gateway"); len(got) > 0 && got[0] == "claude-live-only" {
		t.Fatalf("gateway completion used the public catalog: %v", got)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.com")
	if got := ProviderModelIDs("anthropic"); len(got) > 0 && got[0] == "claude-live-only" {
		t.Fatalf("ANTHROPIC_BASE_URL gateway used the public catalog: %v", got)
	}
}
