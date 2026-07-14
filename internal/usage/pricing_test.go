package usage

import (
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

func TestCalculateCostUsesSambaNovaPricing(t *testing.T) {
	fetcher := NewPricingFetcher()

	cost, err := fetcher.CalculateCost(UsageEntry{
		Model:        "MiniMax-M2.7",
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	})
	if err != nil {
		t.Fatalf("CalculateCost() error = %v", err)
	}
	if math.Abs(cost-3.0) > 1e-9 {
		t.Fatalf("cost = %g, want 3.0", cost)
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
		{"gpt-5.6-sol", 5, 0.5, 6.25, 30, 272_000, true},
		{"openai/gpt-5.6-sol-max", 5, 0.5, 6.25, 30, 272_000, true},
		{"gpt-5.6-terra-high", 2.5, 0.25, 3.125, 15, 272_000, true},
		{"gpt-5.6-luna-medium", 1, 0.1, 1.25, 6, 272_000, true},
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
	wantBase := float64(200_000)*2.5/1_000_000 + float64(70_000)*0.25/1_000_000 + float64(2_000)*3.125/1_000_000 + float64(10_000)*15/1_000_000
	if math.Abs(atThreshold-wantBase) > 1e-12 {
		t.Fatalf("at-threshold cost = %g, want %g", atThreshold, wantBase)
	}

	entry.InputTokens++
	aboveThreshold, err := fetcher.CalculateCost(entry)
	if err != nil {
		t.Fatalf("CalculateCost(above threshold) error = %v", err)
	}
	wantLong := float64(200_001)*5/1_000_000 + float64(70_000)*0.5/1_000_000 + float64(2_000)*6.25/1_000_000 + float64(10_000)*22.5/1_000_000
	if math.Abs(aboveThreshold-wantLong) > 1e-12 {
		t.Fatalf("above-threshold cost = %g, want %g", aboveThreshold, wantLong)
	}
}
