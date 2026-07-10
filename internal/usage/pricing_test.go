package usage

import (
	"math"
	"testing"
)

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
