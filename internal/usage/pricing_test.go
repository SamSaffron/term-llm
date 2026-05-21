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
