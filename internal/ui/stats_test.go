package ui

import "testing"

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
