package ui

import (
	"strings"
	"testing"
	"time"
)

func TestSessionStatsSeedTotals(t *testing.T) {
	stats := NewSessionStats()
	stats.SeedTotals(1000, 250, 700, 0, 3, 2)
	stats.AddUsage(10, 5, 4, 0)

	if stats.InputTokens != 1010 {
		t.Errorf("expected input tokens 1010, got %d", stats.InputTokens)
	}
	if stats.OutputTokens != 255 {
		t.Errorf("expected output tokens 255, got %d", stats.OutputTokens)
	}
	if stats.CachedInputTokens != 704 {
		t.Errorf("expected cached input tokens 704, got %d", stats.CachedInputTokens)
	}
	if stats.ToolCallCount != 3 {
		t.Errorf("expected tool calls 3, got %d", stats.ToolCallCount)
	}
	if stats.LLMCallCount != 3 {
		t.Errorf("expected llm calls 3, got %d", stats.LLMCallCount)
	}
}

func TestAddUsageSetsLastAndPeak(t *testing.T) {
	stats := NewSessionStats()

	// totalContext = input + cached + output = 100 + 0 + 20 = 120
	stats.AddUsage(100, 20, 0, 0)
	if stats.lastInputTokens != 120 {
		t.Errorf("expected lastInputTokens 120 (input+output), got %d", stats.lastInputTokens)
	}
	if stats.lastOutputTokens != 20 {
		t.Errorf("expected lastOutputTokens 20, got %d", stats.lastOutputTokens)
	}
	if stats.peakInputTokens != 120 {
		t.Errorf("expected peakInputTokens 120, got %d", stats.peakInputTokens)
	}

	// Second call with higher context — peak should update
	// totalContext = 500 + 0 + 50 = 550
	stats.AddUsage(500, 50, 0, 0)
	if stats.lastInputTokens != 550 {
		t.Errorf("expected lastInputTokens 550, got %d", stats.lastInputTokens)
	}
	if stats.lastOutputTokens != 50 {
		t.Errorf("expected lastOutputTokens 50, got %d", stats.lastOutputTokens)
	}
	if stats.peakInputTokens != 550 {
		t.Errorf("expected peakInputTokens 550, got %d", stats.peakInputTokens)
	}

	// Third call with lower context — peak should stay at 550
	// totalContext = 200 + 0 + 30 = 230
	stats.AddUsage(200, 30, 0, 0)
	if stats.lastInputTokens != 230 {
		t.Errorf("expected lastInputTokens 230, got %d", stats.lastInputTokens)
	}
	if stats.lastOutputTokens != 30 {
		t.Errorf("expected lastOutputTokens 30, got %d", stats.lastOutputTokens)
	}
	if stats.peakInputTokens != 550 {
		t.Errorf("expected peakInputTokens to remain 550, got %d", stats.peakInputTokens)
	}
}

func TestRenderAfterSeedTotalsWithoutAddUsage(t *testing.T) {
	stats := NewSessionStats()
	stats.SeedTotals(100000, 5000, 80000, 0, 10, 5)

	out := stats.Render()
	if strings.Contains(out, "last:") {
		t.Errorf("expected no last/peak after SeedTotals without AddUsage, got: %s", out)
	}
	if strings.Contains(out, "peak:") {
		t.Errorf("expected no peak after SeedTotals without AddUsage, got: %s", out)
	}
}

func TestSeedTotalsClearsPerCallState(t *testing.T) {
	stats := NewSessionStats()

	// Build up per-call state
	// totalContext = 5000 + 0 + 500 = 5500
	stats.AddUsage(5000, 500, 0, 0)
	stats.AddUsage(2000, 200, 0, 0)
	if stats.peakInputTokens != 5500 {
		t.Fatalf("expected peak 5500 before reseed, got %d", stats.peakInputTokens)
	}

	// Reseed should clear per-call state
	stats.SeedTotals(100000, 10000, 80000, 0, 5, 3)

	if stats.lastInputTokens != 0 {
		t.Errorf("expected lastInputTokens 0 after reseed, got %d", stats.lastInputTokens)
	}
	if stats.lastOutputTokens != 0 {
		t.Errorf("expected lastOutputTokens 0 after reseed, got %d", stats.lastOutputTokens)
	}
	if stats.peakInputTokens != 0 {
		t.Errorf("expected peakInputTokens 0 after reseed, got %d", stats.peakInputTokens)
	}

	// Render should not show last/peak until new AddUsage
	out := stats.Render()
	if strings.Contains(out, "last:") {
		t.Errorf("expected no last/peak after reseed without AddUsage, got: %s", out)
	}

	// New AddUsage should preserve per-call tracking without adding the old,
	// overly detailed last/peak diagnostics to balanced --stats.
	stats.AddUsage(3000, 300, 0, 0)
	out = stats.Render()
	if strings.Contains(out, "last:") || strings.Contains(out, "peak:") {
		t.Errorf("balanced stats should omit last/peak details, got: %s", out)
	}
}

func TestPeakTracksFullContext(t *testing.T) {
	stats := NewSessionStats()

	// totalContext = input + cached + output = 10 + 5000 + 100 = 5110
	// cache_write tokens (2000) are NOT additive — they're a subset of
	// input tokens indicating which ones were written to cache.
	stats.AddUsage(10, 100, 5000, 2000)
	if stats.lastInputTokens != 5110 {
		t.Errorf("expected lastInputTokens 5110 (input+cached+output), got %d", stats.lastInputTokens)
	}
	if stats.peakInputTokens != 5110 {
		t.Errorf("expected peakInputTokens 5110, got %d", stats.peakInputTokens)
	}

	// Second call with smaller total context — peak should stay
	// totalContext = 5 + 100 + 50 = 155
	stats.AddUsage(5, 50, 100, 0)
	if stats.lastInputTokens != 155 {
		t.Errorf("expected lastInputTokens 155, got %d", stats.lastInputTokens)
	}
	if stats.peakInputTokens != 5110 {
		t.Errorf("expected peakInputTokens to remain 5110, got %d", stats.peakInputTokens)
	}
}

func TestRenderCacheLabels(t *testing.T) {
	cases := []struct {
		name        string
		cached      int
		write       int
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "both read and write",
			cached:      500,
			write:       7500,
			wantContain: "500 cached + 7.5K cache write",
		},
		{
			name:        "read only",
			cached:      500,
			write:       0,
			wantContain: "500 cached",
			wantAbsent:  "cache write",
		},
		{
			name:        "write only",
			cached:      0,
			write:       7500,
			wantContain: "7.5K cache write",
			wantAbsent:  "read",
		},
		{
			name:       "no cache",
			cached:     0,
			write:      0,
			wantAbsent: "cache:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats := NewSessionStats()
			stats.InputTokens = 857
			stats.OutputTokens = 803
			stats.CachedInputTokens = tc.cached
			stats.CacheWriteTokens = tc.write

			out := stats.Render()
			if tc.wantContain != "" && !strings.Contains(out, tc.wantContain) {
				t.Errorf("expected %q in render output, got: %s", tc.wantContain, out)
			}
			if tc.wantAbsent != "" && strings.Contains(out, tc.wantAbsent) {
				t.Errorf("did not expect %q in render output, got: %s", tc.wantAbsent, out)
			}
			if !strings.Contains(out, "→") {
				t.Errorf("expected → separator in render output, got: %s", out)
			}
		})
	}
}

func TestRenderBalancedStatsWithTimingCostAndCacheWrite(t *testing.T) {
	stats := NewSessionStats()
	base := time.Now().Add(-12300 * time.Millisecond)
	stats.StartTime = base
	stats.requestStartAt(base)
	stats.outputAt(base.Add(800 * time.Millisecond))
	stats.addUsageAt(24_000, 1_200, 18_000, 2_000, base.Add(1300*time.Millisecond), true)
	stats.ToolCallCount = 3
	stats.LLMCallCount = 4
	stats.SetEstimatedCost(0.084)

	out := stats.Render()
	for _, want := range []string{
		"Stats: 12.3s | 24K in + 18K cached + 2.0K cache write → 1.2K out",
		"TTFT 0.8s, 2400 tok/s",
		"$0.0840",
		"3 tools, 4 calls",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in render output, got: %s", want, out)
		}
	}
}

func TestRenderGracefullyOmitsUnavailablePerformanceCostAndActivity(t *testing.T) {
	stats := NewSessionStats()
	out := stats.Render()
	for _, absent := range []string{"TTFT", "tok/s", "$", "tools", "calls", "last:", "peak:"} {
		if strings.Contains(out, absent) {
			t.Errorf("did not expect %q in render output, got: %s", absent, out)
		}
	}
	if !strings.Contains(out, "0 in → 0 out") {
		t.Errorf("expected token accounting to remain present, got: %s", out)
	}
}

func TestGenerationRateUsesOnlyObservedCompletedCalls(t *testing.T) {
	stats := NewSessionStats()
	base := time.Now().Add(-10 * time.Second)

	// A tool-only generation reports output usage but has no observed generation
	// activity, so it must not inflate throughput.
	stats.requestStartAt(base)
	stats.addUsageAt(10, 900, 0, 0, base.Add(time.Second), true)

	stats.requestStartAt(base.Add(2 * time.Second))
	stats.outputAt(base.Add(3 * time.Second))
	stats.addUsageAt(10, 100, 0, 0, base.Add(5*time.Second), true)

	out := stats.Render()
	if !strings.Contains(out, "50 tok/s") {
		t.Fatalf("rate should use only 100 observed tokens over 2s, got: %s", out)
	}
	if !strings.Contains(out, "TTFT 1.0s") {
		t.Fatalf("TTFT should come from the observed call, got: %s", out)
	}
}

func TestAttemptDiscardWithoutUsageClearsPendingTiming(t *testing.T) {
	stats := NewSessionStats()
	stats.RequestStart()
	stats.ObserveOutput()
	stats.DiscardUsage(0, 0, 0, 0, 0)
	if !stats.requestStartTime.IsZero() || !stats.firstActivityTime.IsZero() || !stats.activityStartTime.IsZero() || stats.activityDuration != 0 {
		t.Fatalf("attempt discard retained pending timing: %+v", stats)
	}
}

func TestDiscardRestoresPerformanceFromRetainedCalls(t *testing.T) {
	stats := NewSessionStats()
	base := time.Now().Add(-10 * time.Second)
	stats.requestStartAt(base)
	stats.outputAt(base.Add(time.Second))
	stats.addUsageAt(10, 100, 0, 0, base.Add(3*time.Second), true) // 50 tok/s
	stats.requestStartAt(base.Add(4 * time.Second))
	stats.outputAt(base.Add(8 * time.Second))
	stats.addUsageAt(10, 1000, 0, 0, base.Add(9*time.Second), true)
	stats.DiscardUsage(10, 1000, 0, 0, 1)

	out := stats.Render()
	if !strings.Contains(out, "TTFT 1.0s, 50 tok/s") {
		t.Fatalf("discard should restore retained-call timing, got: %s", out)
	}
}

func TestDiscardKeepsInterleavedCompactionCall(t *testing.T) {
	stats := NewSessionStats()
	stats.AddUsage(10, 2, 0, 0)
	stats.AddCompactionUsageForModel("compact-model", 20, 3, 0, 0)

	stats.DiscardUsage(10, 2, 0, 0, 1)

	calls, _ := stats.UsageCalls()
	if len(calls) != 1 || !calls[0].Compaction || calls[0].Model != "compact-model" {
		t.Fatalf("discard removed the durable compaction call: %+v", calls)
	}
	if stats.LLMCallCount != 1 || stats.InputTokens != 20 || stats.OutputTokens != 3 {
		t.Fatalf("totals after discard = %+v, want only compaction usage", stats)
	}
}

func TestAddCompactionUsagePreservesPendingMainCallTimingAndModel(t *testing.T) {
	stats := NewSessionStats()
	base := time.Now().Add(-5 * time.Second)
	stats.SetModel("main-model")
	stats.requestStartAt(base)
	stats.outputAt(base.Add(time.Second))
	stats.stopActivityAt(base.Add(2 * time.Second))

	stats.AddCompactionUsageForModel("  compact-model  ", 20, 5, 3, 2)
	if stats.requestStartTime != base || stats.firstActivityTime != base.Add(time.Second) || stats.activityDuration != time.Second {
		t.Fatalf("compaction disturbed pending timing: %+v", stats)
	}

	stats.outputAt(base.Add(3 * time.Second))
	stats.addUsageAt(10, 4, 0, 0, base.Add(4*time.Second), true)
	calls, _ := stats.UsageCalls()
	if len(calls) != 2 {
		t.Fatalf("usage calls = %d, want 2", len(calls))
	}
	if calls[0].Model != "compact-model" || calls[1].Model != "main-model" {
		t.Fatalf("call models = %q, %q", calls[0].Model, calls[1].Model)
	}
	if calls[1].TTFT != time.Second || calls[1].GenerationTime != 2*time.Second {
		t.Fatalf("main-call timing = TTFT %v generation %v", calls[1].TTFT, calls[1].GenerationTime)
	}
}

func TestAllZeroUsageConsumesPendingTimingWithoutRecordingCall(t *testing.T) {
	stats := NewSessionStats()
	stats.RequestStart()
	stats.ObserveOutput()
	stats.AddUsage(0, 0, 0, 0)

	if stats.LLMCallCount != 0 || len(stats.usageCalls) != 0 {
		t.Fatalf("zero usage recorded a meaningful call: %+v", stats)
	}
	if !stats.requestStartTime.IsZero() || !stats.firstActivityTime.IsZero() || !stats.activityStartTime.IsZero() || stats.activityDuration != 0 {
		t.Fatalf("zero usage retained pending timing: %+v", stats)
	}
}

func TestSeedTotalsResetsProcessLocalState(t *testing.T) {
	stats := NewSessionStats()
	stats.SetModel("old")
	stats.RequestStart()
	stats.ObserveOutput()
	stats.AddUsage(1, 1, 0, 0)
	stats.SetEstimatedCost(1)
	stats.SeedTotals(10, 20, 0, 0, 0, 1)
	calls, complete := stats.UsageCalls()
	if complete || len(calls) != 0 {
		t.Fatalf("seeded calls = %v, complete=%v", calls, complete)
	}
	out := stats.Render()
	for _, absent := range []string{"TTFT", "tok/s", "$"} {
		if strings.Contains(out, absent) {
			t.Fatalf("SeedTotals retained %q: %s", absent, out)
		}
	}
}
