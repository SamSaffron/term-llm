package ui

import (
	"strings"
	"testing"
)

func TestSessionStatsSeedTotals(t *testing.T) {
	stats := NewSessionStats()
	stats.SeedTotals(1000, 250, 700, 3, 2)
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
	stats.SeedTotals(100000, 5000, 80000, 10, 5)

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
	stats.SeedTotals(100000, 10000, 80000, 5, 3)

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

	// New AddUsage should work normally after reseed
	stats.AddUsage(3000, 300, 0, 0)
	out = stats.Render()
	if !strings.Contains(out, "last:") {
		t.Errorf("expected last info after AddUsage post-reseed, got: %s", out)
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
			wantContain: "cache: 500 read, 7.5k write",
		},
		{
			name:        "read only",
			cached:      500,
			write:       0,
			wantContain: "cache: 500 read",
			wantAbsent:  "write",
		},
		{
			name:        "write only",
			cached:      0,
			write:       7500,
			wantContain: "cache: 7.5k write",
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

func TestRenderIncludesLastAndPeak(t *testing.T) {
	stats := NewSessionStats()

	// No calls yet — should not include last/peak
	out := stats.Render()
	if strings.Contains(out, "last:") {
		t.Errorf("expected no last/peak before any calls, got: %s", out)
	}

	// Single call — last == peak, so no "peak:" shown
	stats.AddUsage(1000, 200, 0, 0)
	out = stats.Render()
	if !strings.Contains(out, "(last:") {
		t.Errorf("expected last info in render, got: %s", out)
	}
	if strings.Contains(out, "peak:") {
		t.Errorf("expected no peak when last == peak, got: %s", out)
	}

	// Second call with lower input — peak > last, so "peak:" should appear
	stats.AddUsage(500, 100, 0, 0)
	out = stats.Render()
	if !strings.Contains(out, "last:") {
		t.Errorf("expected last info in render, got: %s", out)
	}
	if !strings.Contains(out, "peak:") {
		t.Errorf("expected peak info when peak > last, got: %s", out)
	}
}
