package usage

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const modelsDevFixture = `{
  "agentrouter": {"models": {
    "claude-opus-5": {"cost": {"input": 5, "output": 25}}
  }},
  "anthropic": {"models": {
    "claude-opus-5": {"cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25}}
  }},
  "openai": {"models": {
    "gpt-5.6-sol": {"cost": {"input": 4, "output": 20, "cache_read": 0.4, "cache_write": 5,
      "tiers": [{"input": 8, "output": 30, "cache_read": 0.8, "cache_write": 10, "tier": {"size": 272000, "type": "context"}}]}}
  }},
  "some-reseller": {"models": {
    "claude-opus-5": {"cost": {"input": 50, "output": 250}},
    "free-model": {"cost": {"input": 0, "output": 0}},
    "unpriced-model": {}
  }}
}`

func newDirectoryFetcher(t *testing.T, body string) *PricingFetcher {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	previousURL := modelsDevURL
	modelsDevURL = server.URL
	t.Cleanup(func() { modelsDevURL = previousURL })
	previousStore := modelsDevStore
	modelsDevStore = &modelsDevPricing{}
	t.Cleanup(func() { modelsDevStore = previousStore })

	fetcher := NewPricingFetcher()
	fetcher.cacheDir = t.TempDir()
	return fetcher
}

// The refreshed directory is the source of truth: a rate published after this
// binary was built beats the table compiled into it.
func TestModelsDevDirectoryOverridesBundledPricing(t *testing.T) {
	fetcher := newDirectoryFetcher(t, modelsDevFixture)
	if err := fetcher.RefreshModelsDevPricing(context.Background()); err != nil {
		t.Fatalf("RefreshModelsDevPricing() error = %v", err)
	}

	pricing, err := fetcher.GetPricingLocal("gpt-5.6-sol-max")
	if err != nil {
		t.Fatalf("GetPricingLocal() error = %v", err)
	}
	// Effort suffixes resolve, and a context tier reprices the whole request.
	if pricing.InputCostPerToken != 4.0/1_000_000 || pricing.OutputCostPerToken != 20.0/1_000_000 {
		t.Fatalf("directory rates = %+v", pricing)
	}
	if !pricing.WholeRequestTier || pricing.TieredThreshold != 272_000 ||
		pricing.InputCostPerTokenAbove200k != 8.0/1_000_000 {
		t.Fatalf("directory tier = %+v", pricing)
	}

	// A reseller listing the same model must not claim the bare name.
	cost, err := fetcher.CalculateCostLocal(UsageEntry{Model: "claude-opus-5", InputTokens: 1_000_000})
	if err != nil {
		t.Fatalf("CalculateCostLocal() error = %v", err)
	}
	if math.Abs(cost-5) > 1e-9 {
		t.Fatalf("cost = %v, want the first-party $5", cost)
	}

	// Models with no rates stay unpriced rather than free.
	if _, err := fetcher.GetPricingLocal("unpriced-model"); err == nil {
		t.Fatal("a model with no cost block must not resolve")
	}
	if _, err := fetcher.GetPricingLocal("free-model"); err == nil {
		t.Fatal("a model with zero rates must not resolve")
	}
}

// Rendering a cost must never wait on the network, and a fresh cache must not
// be refetched.
func TestModelsDevRefreshHonoursCacheAndStaysOffline(t *testing.T) {
	fetcher := newDirectoryFetcher(t, modelsDevFixture)
	cacheFile := filepath.Join(fetcher.cacheDir, modelsDevCacheFile)
	cached := `{"version":2,"entries":{"cached-model":{"input_cost_per_token":1e-06,"output_cost_per_token":2e-06}}}`
	if err := os.WriteFile(cacheFile, []byte(cached), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fetcher.RefreshModelsDevPricing(context.Background()); err != nil {
		t.Fatalf("RefreshModelsDevPricing() error = %v", err)
	}
	// The fresh cache was kept, not replaced by the server fixture.
	pricing, err := fetcher.GetPricingLocal("cached-model")
	if err != nil || pricing.InputCostPerToken != 1e-06 {
		t.Fatalf("cached directory not used: %+v err=%v", pricing, err)
	}

	// Once it ages out, the directory is refetched.
	old := time.Now().Add(-modelsDevTTL - time.Minute)
	if err := os.Chtimes(cacheFile, old, old); err != nil {
		t.Fatal(err)
	}
	if err := fetcher.RefreshModelsDevPricing(context.Background()); err != nil {
		t.Fatalf("RefreshModelsDevPricing() error = %v", err)
	}
	var rewritten modelsDevCacheFileFormat
	data, err := os.ReadFile(cacheFile)
	if err != nil || json.Unmarshal(data, &rewritten) != nil {
		t.Fatalf("refresh did not rewrite the cache: %v", err)
	}
	if rewritten.Version != modelsDevCacheVersion {
		t.Fatalf("cache version = %d, want %d", rewritten.Version, modelsDevCacheVersion)
	}
	if _, ok := rewritten.Entries["claude-opus-5"]; !ok {
		t.Fatalf("refreshed cache = %v", rewritten.Entries)
	}

	// A projection written by an older build is discarded rather than trusted:
	// the rules that derived it have changed.
	if err := os.WriteFile(cacheFile, []byte(`{"version":1,"entries":{"stale-model":{"input_cost_per_token":9e-06}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	modelsDevStore = &modelsDevPricing{}
	if _, err := fetcher.GetPricingLocal("stale-model"); err == nil {
		t.Fatal("a projection from an older build must not be used")
	}
}

// A reseller that relists a model usually publishes input and output only.
// Letting it claim the bare name would silently drop the cache rates that
// dominate an agent's bill, so first-party providers claim it first.
func TestModelsDevPrefersFirstPartyRates(t *testing.T) {
	fetcher := newDirectoryFetcher(t, modelsDevFixture)
	if err := fetcher.RefreshModelsDevPricing(context.Background()); err != nil {
		t.Fatalf("RefreshModelsDevPricing() error = %v", err)
	}
	pricing, err := fetcher.GetPricingLocal("claude-opus-5")
	if err != nil {
		t.Fatalf("GetPricingLocal() error = %v", err)
	}
	if pricing.CacheReadInputTokenCost != 0.5/1_000_000 || pricing.CacheCreationInputTokenCost != 6.25/1_000_000 {
		t.Fatalf("bare name resolved to a reseller: %+v", pricing)
	}
	// The reseller is still addressable by its qualified name, and the cache
	// rates it does not publish fall back to the known card rather than reading
	// as free.
	qualified, err := fetcher.GetPricingLocal("some-reseller/claude-opus-5")
	if err != nil {
		t.Fatalf("qualified reseller lookup: %v", err)
	}
	if qualified.InputCostPerToken != 50.0/1_000_000 || qualified.CacheReadInputTokenCost == 0 {
		t.Fatalf("qualified reseller lookup = %+v", qualified)
	}
}

// A catalog entry that states input and output but omits cache rates would
// otherwise price cache reads at zero — most of an agent's bill.
func TestModelsDevKeepsBundledCacheRatesWhenTheCatalogOmitsThem(t *testing.T) {
	fetcher := newDirectoryFetcher(t, `{"anthropic": {"models": {
		"opus": {"cost": {"input": 5, "output": 25}}
	}}}`)
	if err := fetcher.RefreshModelsDevPricing(context.Background()); err != nil {
		t.Fatalf("RefreshModelsDevPricing() error = %v", err)
	}
	pricing, err := fetcher.GetPricingLocal("opus")
	if err != nil {
		t.Fatalf("GetPricingLocal() error = %v", err)
	}
	if pricing.InputCostPerToken != 5.0/1_000_000 {
		t.Fatalf("directory rate ignored: %+v", pricing)
	}
	bundled, _ := lookupBundledPricing("opus")
	if pricing.CacheReadInputTokenCost != bundled.CacheReadInputTokenCost ||
		pricing.CacheCreationInputTokenCost != bundled.CacheCreationInputTokenCost {
		t.Fatalf("cache rates zeroed by an incomplete catalog entry: %+v", pricing)
	}
}
