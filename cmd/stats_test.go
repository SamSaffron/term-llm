package cmd

import (
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestCompactionUsageCollectorMergesWithoutSession(t *testing.T) {
	var collector compactionUsageCollector
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			collector.add(&llm.CompactionResult{Model: "  gpt-5.6-sol  ", Usage: llm.Usage{
				InputTokens: 7, OutputTokens: 3, CachedInputTokens: 11, CacheWriteTokens: 2,
			}})
		}()
	}
	wg.Wait()
	stats := ui.NewSessionStats()
	collector.merge(stats)
	if stats.InputTokens != 56 || stats.OutputTokens != 24 || stats.CachedInputTokens != 88 || stats.CacheWriteTokens != 16 || stats.LLMCallCount != 8 || stats.CompactionLLMCallCount != 8 {
		t.Fatalf("compaction usage not merged into --stats: %+v", stats)
	}
	calls, _ := stats.UsageCalls()
	for _, call := range calls {
		if call.Model != "gpt-5.6-sol" {
			t.Fatalf("compaction model = %q, want normalized actual model", call.Model)
		}
	}
}

func TestSetEstimatedStatsCostSumsRequestScopedTierPricing(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SetModel("gpt-5.6-sol")
	// Each request is below the 272K whole-request tier even though their
	// aggregate is above it. Each must therefore retain base pricing.
	stats.AddUsage(200_000, 10_000, 0, 0)
	stats.AddUsage(200_000, 10_000, 0, 0)

	setEstimatedStatsCost(stats, "")
	out := stats.Render()
	// 400K input at $5/M + 20K output at $30/M = $2.60.
	if !strings.Contains(out, "$2.6000") {
		t.Fatalf("stats cost was not summed per request: %s", out)
	}
}

func TestSetEstimatedStatsCostTracksModelSwitchPerCall(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SetModel("gpt-5.6-sol")
	stats.AddUsage(100_000, 0, 0, 0) // $0.50
	stats.SetModel("gpt-5.6-luna")
	stats.AddUsage(100_000, 0, 0, 0) // $0.10
	setEstimatedStatsCost(stats, "gpt-5.6-luna")
	if out := stats.Render(); !strings.Contains(out, "$0.6000") {
		t.Fatalf("model-switched calls were not priced independently: %s", out)
	}
}

func TestSetEstimatedStatsCostOmitsResumedHistoricalUsage(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SeedTotals(100, 10, 0, 0, 0, 1)
	stats.SetModel("gpt-5.6-sol")
	stats.AddUsage(100, 10, 0, 0)
	setEstimatedStatsCost(stats, "gpt-5.6-sol")
	if out := stats.Render(); strings.Contains(out, "$") {
		t.Fatalf("resumed whole-session cost should be omitted: %s", out)
	}
}
