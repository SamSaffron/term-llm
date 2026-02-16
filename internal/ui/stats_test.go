package ui

import (
	"strings"
	"testing"
)

func TestSessionStatsSeedTotals(t *testing.T) {
	stats := NewSessionStats()
	stats.SeedTotals(1000, 250, 700, 3, 2)
	stats.AddUsage(10, 5, 4)

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

	stats.AddUsage(100, 20, 0)
	if stats.lastInputTokens != 100 {
		t.Errorf("expected lastInputTokens 100, got %d", stats.lastInputTokens)
	}
	if stats.lastOutputTokens != 20 {
		t.Errorf("expected lastOutputTokens 20, got %d", stats.lastOutputTokens)
	}
	if stats.peakInputTokens != 100 {
		t.Errorf("expected peakInputTokens 100, got %d", stats.peakInputTokens)
	}

	// Second call with higher input — peak should update
	stats.AddUsage(500, 50, 0)
	if stats.lastInputTokens != 500 {
		t.Errorf("expected lastInputTokens 500, got %d", stats.lastInputTokens)
	}
	if stats.lastOutputTokens != 50 {
		t.Errorf("expected lastOutputTokens 50, got %d", stats.lastOutputTokens)
	}
	if stats.peakInputTokens != 500 {
		t.Errorf("expected peakInputTokens 500, got %d", stats.peakInputTokens)
	}

	// Third call with lower input — peak should stay at 500
	stats.AddUsage(200, 30, 0)
	if stats.lastInputTokens != 200 {
		t.Errorf("expected lastInputTokens 200, got %d", stats.lastInputTokens)
	}
	if stats.lastOutputTokens != 30 {
		t.Errorf("expected lastOutputTokens 30, got %d", stats.lastOutputTokens)
	}
	if stats.peakInputTokens != 500 {
		t.Errorf("expected peakInputTokens to remain 500, got %d", stats.peakInputTokens)
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
	stats.AddUsage(5000, 500, 0)
	stats.AddUsage(2000, 200, 0)
	if stats.peakInputTokens != 5000 {
		t.Fatalf("expected peak 5000 before reseed, got %d", stats.peakInputTokens)
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
	stats.AddUsage(3000, 300, 0)
	out = stats.Render()
	if !strings.Contains(out, "last:") {
		t.Errorf("expected last info after AddUsage post-reseed, got: %s", out)
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
	stats.AddUsage(1000, 200, 0)
	out = stats.Render()
	if !strings.Contains(out, "(last:") {
		t.Errorf("expected last info in render, got: %s", out)
	}
	if strings.Contains(out, "peak:") {
		t.Errorf("expected no peak when last == peak, got: %s", out)
	}

	// Second call with lower input — peak > last, so "peak:" should appear
	stats.AddUsage(500, 100, 0)
	out = stats.Render()
	if !strings.Contains(out, "last:") {
		t.Errorf("expected last info in render, got: %s", out)
	}
	if !strings.Contains(out, "peak:") {
		t.Errorf("expected peak info when peak > last, got: %s", out)
	}
}
