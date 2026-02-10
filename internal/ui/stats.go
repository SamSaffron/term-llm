package ui

import (
	"fmt"
	"time"
)

// SessionStats tracks statistics for a session.
type SessionStats struct {
	StartTime         time.Time
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int // Tokens read from cache
	ToolCallCount     int
	LLMCallCount      int // Number of LLM API calls made

	// Time tracking
	LLMTime       time.Duration
	ToolTime      time.Duration
	lastEventTime time.Time
	inTool        bool
}

// NewSessionStats creates a new SessionStats with StartTime set to now.
func NewSessionStats() *SessionStats {
	now := time.Now()
	return &SessionStats{
		StartTime:     now,
		lastEventTime: now,
	}
}

// SeedTotals initializes cumulative counters from persisted session metrics.
// Timing remains scoped to the current process run.
func (s *SessionStats) SeedTotals(input, output, cached, toolCalls, llmCalls int) {
	s.InputTokens = input
	s.OutputTokens = output
	s.CachedInputTokens = cached
	s.ToolCallCount = toolCalls
	s.LLMCallCount = llmCalls
}

// AddUsage adds token usage to the stats and increments the LLM call count.
func (s *SessionStats) AddUsage(input, output, cached int) {
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
	s.LLMCallCount++
}

// ToolStart marks the start of a tool execution.
func (s *SessionStats) ToolStart() {
	now := time.Now()
	if !s.inTool {
		// Was in LLM phase, record LLM time
		s.LLMTime += now.Sub(s.lastEventTime)
	}
	s.lastEventTime = now
	s.inTool = true
	s.ToolCallCount++
}

// ToolEnd marks the end of tool execution (back to LLM).
func (s *SessionStats) ToolEnd() {
	now := time.Now()
	if s.inTool {
		// Was in tool phase, record tool time
		s.ToolTime += now.Sub(s.lastEventTime)
	}
	s.lastEventTime = now
	s.inTool = false
}

// Finalize records any remaining time.
func (s *SessionStats) Finalize() {
	now := time.Now()
	if s.inTool {
		s.ToolTime += now.Sub(s.lastEventTime)
	} else {
		s.LLMTime += now.Sub(s.lastEventTime)
	}
	s.lastEventTime = now
}

// Render returns the stats as a compact single-line string.
func (s SessionStats) Render() string {
	total := time.Since(s.StartTime)

	// Format tokens with optional cache info
	var tokensStr string
	if s.CachedInputTokens > 0 {
		tokensStr = fmt.Sprintf("%s in (%s cached) / %s out",
			FormatTokenCount(s.InputTokens),
			FormatTokenCount(s.CachedInputTokens),
			FormatTokenCount(s.OutputTokens))
	} else {
		tokensStr = fmt.Sprintf("%s in / %s out",
			FormatTokenCount(s.InputTokens),
			FormatTokenCount(s.OutputTokens))
	}

	// Format time breakdown
	var timeStr string
	if s.ToolCallCount > 0 {
		timeStr = fmt.Sprintf("%.1fs (llm %.1fs + tool %.1fs)",
			total.Seconds(), s.LLMTime.Seconds(), s.ToolTime.Seconds())
	} else {
		timeStr = fmt.Sprintf("%.1fs", total.Seconds())
	}

	return fmt.Sprintf("Stats: %s | %s | %d tools | %d llm calls",
		timeStr, tokensStr, s.ToolCallCount, s.LLMCallCount)
}
