package usage

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCalculateCostLocalUsesStaleCacheWithoutNetwork(t *testing.T) {
	cacheDir := t.TempDir()
	cacheFile := filepath.Join(cacheDir, "pricing.json")
	if err := os.WriteFile(cacheFile, []byte(`{"local-model":{"input_cost_per_token":0.000001}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(cacheFile, stale, stale); err != nil {
		t.Fatal(err)
	}
	fetcher := NewPricingFetcher()
	fetcher.cacheDir = cacheDir
	// No network client is needed: a network attempt would panic on nil.
	fetcher.httpClient = nil
	cost, err := fetcher.CalculateCostLocal(UsageEntry{Model: "local-model", InputTokens: 1000})
	if err != nil {
		t.Fatalf("CalculateCostLocal() error = %v", err)
	}
	if math.Abs(cost-0.001) > 1e-9 {
		t.Fatalf("cost = %g, want .001", cost)
	}
}

func TestGetPricingLocalLoadsDiskCacheAtMostOnce(t *testing.T) {
	cacheDir := t.TempDir()
	cacheFile := filepath.Join(cacheDir, "pricing.json")
	if err := os.WriteFile(cacheFile, []byte(`{"model-a":{"input_cost_per_token":0.000001},"model-b":{"input_cost_per_token":0.000002}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fetcher := NewPricingFetcher()
	fetcher.cacheDir = cacheDir
	if _, err := fetcher.GetPricingLocal("model-a"); err != nil {
		t.Fatalf("first GetPricingLocal() error = %v", err)
	}
	if err := os.Remove(cacheFile); err != nil {
		t.Fatal(err)
	}
	pricing, err := fetcher.GetPricingLocal("model-b")
	if err != nil {
		t.Fatalf("second GetPricingLocal() reread disk cache: %v", err)
	}
	if pricing.InputCostPerToken != 0.000002 {
		t.Fatalf("model-b input price = %g", pricing.InputCostPerToken)
	}
}

func TestGetPricingLocalMatchesGetPricingDeterministicPrecedence(t *testing.T) {
	cacheDir := t.TempDir()
	data := []byte(`{
		"anthropic/model-x":{"input_cost_per_token":0.000002},
		"aaa-model-x-extended":{"input_cost_per_token":0.000003},
		"zzz-model-x-extended":{"input_cost_per_token":0.000004},
		"aaa-fuzzy-name":{"input_cost_per_token":0.000005},
		"zzz-fuzzy-name":{"input_cost_per_token":0.000006}
	}`)
	if err := os.WriteFile(filepath.Join(cacheDir, "pricing.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	local := NewPricingFetcher()
	local.cacheDir = cacheDir
	network := NewPricingFetcher()
	if err := network.parseData(data); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		model string
		want  float64
	}{
		{model: "model-x", want: 0.000002}, // provider-prefix match beats partial matches
		{model: "fuzzy", want: 0.000005},   // ambiguous partial match is lexical, not map-order dependent
	} {
		localPricing, err := local.GetPricingLocal(tc.model)
		if err != nil {
			t.Fatalf("GetPricingLocal(%q): %v", tc.model, err)
		}
		regularPricing, err := network.GetPricing(tc.model)
		if err != nil {
			t.Fatalf("GetPricing(%q): %v", tc.model, err)
		}
		if localPricing.InputCostPerToken != tc.want || regularPricing.InputCostPerToken != tc.want {
			t.Fatalf("pricing(%q) local=%g regular=%g, want %g", tc.model, localPricing.InputCostPerToken, regularPricing.InputCostPerToken, tc.want)
		}
	}
}

func TestCalculateCostLocalGracefullyFailsWithoutCache(t *testing.T) {
	fetcher := NewPricingFetcher()
	fetcher.cacheDir = t.TempDir()
	fetcher.httpClient = nil
	if _, err := fetcher.CalculateCostLocal(UsageEntry{Model: "unknown", InputTokens: 1}); err == nil {
		t.Fatal("expected unavailable local pricing error")
	}
}

func TestPricingFetcherUsesBundledSambaNovaPricing(t *testing.T) {
	fetcher := NewPricingFetcher()

	pricing, err := fetcher.GetPricing("gpt-oss-120b")
	if err != nil {
		t.Fatalf("GetPricing() error = %v", err)
	}
	wantInput := 0.22 / 1_000_000
	wantOutput := 0.59 / 1_000_000
	if pricing.InputCostPerToken != wantInput || pricing.OutputCostPerToken != wantOutput {
		t.Fatalf("pricing = %g/%g, want %g/%g", pricing.InputCostPerToken, pricing.OutputCostPerToken, wantInput, wantOutput)
	}
}

func TestCalculateCostUsesBundledMiniMaxPricing(t *testing.T) {
	fetcher := NewPricingFetcher()

	cost, err := fetcher.CalculateCost(UsageEntry{
		Model:        "MiniMax-M2.7",
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	})
	if err != nil {
		t.Fatalf("CalculateCost() error = %v", err)
	}
	// MiniMax first-party list pricing, not the SambaNova reseller rate.
	if math.Abs(cost-1.5) > 1e-9 {
		t.Fatalf("cost = %g, want 1.5", cost)
	}
}

func TestGPT56BundledPricingAndEffortAliases(t *testing.T) {
	fetcher := NewPricingFetcher()
	tests := []struct {
		model            string
		wantInput        float64
		wantCacheRead    float64
		wantCacheWrite   float64
		wantOutput       float64
		wantThreshold    int
		wantWholeRequest bool
	}{
		{"gpt-6-astra", 10, 1, 12.5, 50, 272_000, true},
		{"gpt-5.6-sol", 4, 0.4, 5, 20, 272_000, true},
		{"openai/gpt-5.6-sol-max", 4, 0.4, 5, 20, 272_000, true},
		{"gpt-5.6-terra-high", 2, 0.2, 2.5, 12, 272_000, true},
		{"gpt-5.6-luna-medium", 0.2, 0.02, 0.25, 1.2, 272_000, true},
		// xAI and Google reprice the whole request at their own thresholds.
		{"grok-4.6-high", 2, 0.5, 0, 6, 200_000, true},
		{"gemini-3.1-pro-high", 2, 0.2, 0, 12, 200_000, true},
		// Flash and the flat-rate vendors have no tier at all.
		{"gemini-3.8-flash-high", 0.75, 0.075, 0, 3.75, 0, false},
		{"glm-5.3-max", 1.4, 0.26, 0, 4.4, 0, false},
		{"kimi-k3", 3, 0.3, 0, 15, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, err := fetcher.GetPricing(tt.model)
			if err != nil {
				t.Fatalf("GetPricing() error = %v", err)
			}
			perMillion := func(v float64) float64 { return v / 1_000_000 }
			if got.InputCostPerToken != perMillion(tt.wantInput) ||
				got.CacheReadInputTokenCost != perMillion(tt.wantCacheRead) ||
				got.CacheCreationInputTokenCost != perMillion(tt.wantCacheWrite) ||
				got.OutputCostPerToken != perMillion(tt.wantOutput) ||
				got.TieredThreshold != tt.wantThreshold || got.WholeRequestTier != tt.wantWholeRequest {
				t.Fatalf("pricing = %+v", got)
			}
		})
	}
}

func TestGPT56LongContextPricingRepricesWholeRequest(t *testing.T) {
	fetcher := NewPricingFetcher()
	entry := UsageEntry{
		Model:            "gpt-5.6-terra",
		InputTokens:      200_000,
		CacheReadTokens:  70_000,
		CacheWriteTokens: 2_000,
		OutputTokens:     10_000,
	}
	atThreshold, err := fetcher.CalculateCost(entry)
	if err != nil {
		t.Fatalf("CalculateCost(at threshold) error = %v", err)
	}
	wantBase := float64(200_000)*2/1_000_000 + float64(70_000)*0.2/1_000_000 + float64(2_000)*2.5/1_000_000 + float64(10_000)*12/1_000_000
	if math.Abs(atThreshold-wantBase) > 1e-12 {
		t.Fatalf("at-threshold cost = %g, want %g", atThreshold, wantBase)
	}

	entry.InputTokens++
	aboveThreshold, err := fetcher.CalculateCost(entry)
	if err != nil {
		t.Fatalf("CalculateCost(above threshold) error = %v", err)
	}
	wantLong := float64(200_001)*4/1_000_000 + float64(70_000)*0.4/1_000_000 + float64(2_000)*5/1_000_000 + float64(10_000)*18/1_000_000
	if math.Abs(aboveThreshold-wantLong) > 1e-12 {
		t.Fatalf("above-threshold cost = %g, want %g", aboveThreshold, wantLong)
	}
}

func TestBundledAnthropicPricingResolvesWithoutCache(t *testing.T) {
	fetcher := NewPricingFetcher()
	fetcher.cacheDir = t.TempDir() // Empty: no pricing.json exists on disk.
	// No network client is needed: bundled pricing must short-circuit before any
	// fetch, so a network attempt would panic on nil.
	fetcher.httpClient = nil

	perMillion := func(v float64) float64 { return v / 1_000_000 }
	tests := []struct {
		model          string
		wantInput      float64
		wantCacheWrite float64
		wantCacheRead  float64
		wantOutput     float64
	}{
		// Bare aliases follow the CLI's current model for that family.
		{"opus", 5, 6.25, 0.50, 25},
		{"opus-max", 5, 6.25, 0.50, 25},
		{"sonnet", 2, 2.50, 0.20, 10},
		{"fable", 10, 12.50, 1.00, 50},
		{"fable-medium", 10, 12.50, 1.00, 50},
		{"haiku", 1, 1.25, 0.10, 5},
		{"claude-opus-5", 5, 6.25, 0.50, 25},
		{"claude-opus-4-8", 5, 6.25, 0.50, 25},
		// Superseded generations keep their own rates so historical sessions
		// are not repriced at today's card.
		{"anthropic/claude-opus-4-5", 15, 18.75, 1.50, 75},
		{"claude-opus-4-7", 15, 18.75, 1.50, 75},
		{"claude-sonnet-5", 2, 2.50, 0.20, 10},
		{"claude-sonnet-4-6", 3, 3.75, 0.30, 15},
		{"claude-fable-5", 10, 12.50, 1.00, 50},
		{"claude-haiku-4-5", 1, 1.25, 0.10, 5},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, err := fetcher.GetPricingLocal(tt.model)
			if err != nil {
				t.Fatalf("GetPricingLocal() error = %v", err)
			}
			if got.InputCostPerToken != perMillion(tt.wantInput) ||
				got.CacheCreationInputTokenCost != perMillion(tt.wantCacheWrite) ||
				got.CacheReadInputTokenCost != perMillion(tt.wantCacheRead) ||
				got.OutputCostPerToken != perMillion(tt.wantOutput) {
				t.Fatalf("pricing = %+v", got)
			}
			if got.InputCostPerTokenAbove200k != 0 || got.OutputCostPerTokenAbove200k != 0 ||
				got.CacheCreationCostAbove200k != 0 || got.CacheReadCostAbove200k != 0 ||
				got.TieredThreshold != 0 || got.WholeRequestTier {
				t.Fatalf("pricing unexpectedly tiered = %+v", got)
			}

			// GetPricing must also short-circuit on bundled data instead of
			// reaching for the network or the (absent) disk cache.
			regular, err := fetcher.GetPricing(tt.model)
			if err != nil {
				t.Fatalf("GetPricing() error = %v", err)
			}
			if regular != got {
				t.Fatalf("GetPricing() = %+v, want %+v", regular, got)
			}
		})
	}

	entries, err := os.ReadDir(fetcher.cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("bundled pricing wrote to the cache dir: %v", entries)
	}
}

func TestCalculateCostLocalUsesBundledAnthropicPricing(t *testing.T) {
	fetcher := NewPricingFetcher()
	fetcher.cacheDir = t.TempDir()
	fetcher.httpClient = nil

	// Cache reads above the legacy 200K threshold are still charged the flat
	// base rate: Claude pricing has no long-context tier.
	cost, err := fetcher.CalculateCostLocal(UsageEntry{
		Model:            "opus",
		InputTokens:      1_000,
		OutputTokens:     2_000,
		CacheReadTokens:  500_000,
		CacheWriteTokens: 10_000,
	})
	if err != nil {
		t.Fatalf("CalculateCostLocal() error = %v", err)
	}
	want := float64(1_000)*5/1_000_000 + float64(2_000)*25/1_000_000 +
		float64(500_000)*0.50/1_000_000 + float64(10_000)*6.25/1_000_000 // 0.005 + 0.05 + 0.25 + 0.0625 = $0.3675
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost = %g, want %g", cost, want)
	}

	// Fable bills at its own premium rates, not Sonnet's.
	cost, err = fetcher.CalculateCostLocal(UsageEntry{
		Model:        "fable-medium",
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	})
	if err != nil {
		t.Fatalf("CalculateCostLocal() error = %v", err)
	}
	if math.Abs(cost-60) > 1e-12 {
		t.Fatalf("cost = %g, want 60", cost)
	}
}

func TestPricingCacheDirHonoursXDGCacheHome(t *testing.T) {
	xdgCache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdgCache)

	fetcher := NewPricingFetcher()
	want := filepath.Join(xdgCache, "term-llm")
	if fetcher.cacheDir != want {
		t.Fatalf("cacheDir = %q, want %q", fetcher.cacheDir, want)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("cache dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("cache dir %q is not a directory", want)
	}

	// The resolved directory is what the cache file is read from.
	cacheFile := filepath.Join(want, "pricing.json")
	if err := os.WriteFile(cacheFile, []byte(`{"xdg-model":{"input_cost_per_token":0.000004}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pricing, err := fetcher.GetPricingLocal("xdg-model")
	if err != nil {
		t.Fatalf("GetPricingLocal() error = %v", err)
	}
	if pricing.InputCostPerToken != 0.000004 {
		t.Fatalf("input price = %g", pricing.InputCostPerToken)
	}
}

func TestPricingCacheDirFallsBackToHomeCache(t *testing.T) {
	home := t.TempDir()
	// HOME covers Unix, USERPROFILE covers Windows.
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	fetcher := NewPricingFetcher()
	want := filepath.Join(home, ".cache", "term-llm")
	if fetcher.cacheDir != want {
		t.Fatalf("cacheDir = %q, want %q", fetcher.cacheDir, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("cache dir %q not created: %v", want, err)
	}
}

func TestResolvePricingCacheDirFallbackOrder(t *testing.T) {
	xdgCache := filepath.Join("xdg", "cache")
	homeDir := filepath.Join("home", "user")
	tempDir := filepath.Join("temp", "dir")
	xdgCandidate := filepath.Join(xdgCache, "term-llm")
	homeCandidate := filepath.Join(homeDir, ".cache", "term-llm")
	tempCandidate := filepath.Join(tempDir, "term-llm-pricing")

	tests := []struct {
		name     string
		xdg      string
		home     string
		temp     string
		unusable []string
		want     string
	}{
		{
			name: "xdg wins when it can be created",
			xdg:  xdgCache,
			home: homeDir,
			temp: tempDir,
			want: xdgCandidate,
		},
		{
			name:     "unusable xdg falls back to home",
			xdg:      xdgCache,
			home:     homeDir,
			temp:     tempDir,
			unusable: []string{xdgCandidate},
			want:     homeCandidate,
		},
		{
			name:     "unknown home falls back to temp",
			xdg:      xdgCache,
			temp:     tempDir,
			unusable: []string{xdgCandidate},
			want:     tempCandidate,
		},
		{
			name:     "unusable xdg and home fall back to temp",
			xdg:      xdgCache,
			home:     homeDir,
			temp:     tempDir,
			unusable: []string{xdgCandidate, homeCandidate},
			want:     tempCandidate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mkdirAll := func(dir string) error {
				for _, unusable := range tt.unusable {
					if dir == unusable {
						return fmt.Errorf("mkdir %s: permission denied", dir)
					}
				}
				return nil
			}
			if got := resolvePricingCacheDir(tt.xdg, tt.home, tt.temp, mkdirAll); got != tt.want {
				t.Fatalf("resolvePricingCacheDir(%q, %q, %q) = %q, want %q", tt.xdg, tt.home, tt.temp, got, tt.want)
			}
		})
	}
}

// Summed counters carry no request boundaries, so no long-context tier may be
// attributed to them: a session of small requests must not be billed as one
// enormous request at up to double the rate.
func TestAggregatePricingNeverAppliesLongContextTiers(t *testing.T) {
	fetcher := NewPricingFetcher()
	fetcher.cacheDir = t.TempDir()
	fetcher.httpClient = nil

	// Far past the 272K threshold in total, but no single request was.
	entry := UsageEntry{Model: "gpt-5.6-sol", CacheReadTokens: 36_000_000}
	aggregate, err := fetcher.CalculateCostLocal(withAggregate(entry))
	if err != nil {
		t.Fatalf("CalculateCostLocal() error = %v", err)
	}
	want := float64(36_000_000) * 0.40 / 1_000_000 // base cache-read rate
	if math.Abs(aggregate-want) > 1e-9 {
		t.Fatalf("aggregate cost = %g, want the base rate %g", aggregate, want)
	}

	// The same counters priced as one real request do cross the tier.
	perRequest, err := fetcher.CalculateCostLocal(entry)
	if err != nil {
		t.Fatalf("CalculateCostLocal() error = %v", err)
	}
	if math.Abs(perRequest-want*2) > 1e-9 {
		t.Fatalf("single-request cost = %g, want the tiered %g", perRequest, want*2)
	}
}

func withAggregate(entry UsageEntry) UsageEntry {
	entry.PreAggregated = true
	return entry
}
