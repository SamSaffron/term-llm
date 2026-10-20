package runtime

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/ui"
)

func TestSetEstimatedStatsCostSumsRequestScopedTierPricing(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SetModel("gpt-5.6-sol")
	// Each request is below the 272K whole-request tier even though their
	// aggregate is above it. Each must therefore retain base pricing.
	stats.AddUsage(200_000, 10_000, 0, 0)
	stats.AddUsage(200_000, 10_000, 0, 0)

	SetEstimatedStatsCost(stats, "")
	out := stats.Render()
	// 400K input at $4/M + 20K output at $20/M = $2.00.
	if !strings.Contains(out, "$2.0000") {
		t.Fatalf("stats cost was not summed per request: %s", out)
	}
}

func TestSetEstimatedStatsCostTracksModelSwitchPerCall(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SetModel("gpt-5.6-sol")
	stats.AddUsage(100_000, 0, 0, 0) // $0.40
	stats.SetModel("gpt-5.6-luna")
	stats.AddUsage(100_000, 0, 0, 0) // $0.02
	SetEstimatedStatsCost(stats, "gpt-5.6-luna")
	if out := stats.Render(); !strings.Contains(out, "$0.4200") {
		t.Fatalf("model-switched calls were not priced independently: %s", out)
	}
}

func TestSetEstimatedStatsCostPricesGuardianWithOwnModel(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SetModel("gpt-5.6-sol")
	stats.AddUsage(100_000, 0, 0, 0)                                 // $0.40
	stats.AddGuardianUsageForModel("gpt-5.6-luna", 100_000, 0, 0, 0) // $0.02

	SetEstimatedStatsCost(stats, "gpt-5.6-sol")
	if out := stats.Render(); !strings.Contains(out, "$0.4200") {
		t.Fatalf("guardian call was not priced with its own model: %s", out)
	}
}

func TestSetEstimatedStatsCostOmitsResumedHistoricalUsage(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SeedTotals(100, 10, 0, 0, 0, 1)
	stats.SetModel("gpt-5.6-sol")
	stats.AddUsage(100, 10, 0, 0)
	SetEstimatedStatsCost(stats, "gpt-5.6-sol")
	if out := stats.Render(); strings.Contains(out, "$") {
		t.Fatalf("resumed whole-session cost should be omitted: %s", out)
	}
}
