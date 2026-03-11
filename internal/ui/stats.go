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
	CacheWriteTokens  int // Tokens written to cache
	ToolCallCount     int
	LLMCallCount      int // Number of LLM API calls made

	// Per-call tracking (current process only, not seeded from persisted data)
	lastInputTokens  int  // Input tokens from most recent LLM call
	lastOutputTokens int  // Output tokens from most recent LLM call
	peakInputTokens  int  // Highest input tokens seen in any single LLM call
	hasPerCallUsage  bool // True after first AddUsage in this process

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
func (s *SessionStats) SeedTotals(input, output, cached, cacheWrite, toolCalls, llmCalls int) {
	s.InputTokens = input
	s.OutputTokens = output
	s.CachedInputTokens = cached
	s.CacheWriteTokens = cacheWrite
	s.ToolCallCount = toolCalls
	s.LLMCallCount = llmCalls
	// Reset per-call fields so stale data from prior usage doesn't leak through
	s.lastInputTokens = 0
	s.lastOutputTokens = 0
	s.peakInputTokens = 0
	s.hasPerCallUsage = false
}

// AddUsage adds token usage to the stats and increments the LLM call count.
func (s *SessionStats) AddUsage(input, output, cached, cacheWrite int) {
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
	s.CacheWriteTokens += cacheWrite
	s.LLMCallCount++
	totalContext := input + cached + output
	s.lastInputTokens = totalContext
	s.lastOutputTokens = output
	s.hasPerCallUsage = true
	if totalContext > s.peakInputTokens {
		s.peakInputTokens = totalContext
	}
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
	switch {
	case s.CachedInputTokens > 0 && s.CacheWriteTokens > 0:
		tokensStr = fmt.Sprintf("%s in (cache: %s read, %s write) → %s out",
			FormatTokenCount(s.InputTokens),
			FormatTokenCount(s.CachedInputTokens),
			FormatTokenCount(s.CacheWriteTokens),
			FormatTokenCount(s.OutputTokens))
	case s.CachedInputTokens > 0:
		tokensStr = fmt.Sprintf("%s in (cache: %s read) → %s out",
			FormatTokenCount(s.InputTokens),
			FormatTokenCount(s.CachedInputTokens),
			FormatTokenCount(s.OutputTokens))
	case s.CacheWriteTokens > 0:
		tokensStr = fmt.Sprintf("%s in (cache: %s write) → %s out",
			FormatTokenCount(s.InputTokens),
			FormatTokenCount(s.CacheWriteTokens),
			FormatTokenCount(s.OutputTokens))
	default:
		tokensStr = fmt.Sprintf("%s in → %s out",
			FormatTokenCount(s.InputTokens),
			FormatTokenCount(s.OutputTokens))
	}

	// Append last/peak context info when there's been at least one AddUsage this process
	if s.hasPerCallUsage {
		lastStr := fmt.Sprintf("last: %s→%s",
			FormatTokenCount(s.lastInputTokens),
			FormatTokenCount(s.lastOutputTokens))
		if s.peakInputTokens > s.lastInputTokens {
			tokensStr += fmt.Sprintf(" (%s, peak: %s)", lastStr, FormatTokenCount(s.peakInputTokens))
		} else {
			tokensStr += fmt.Sprintf(" (%s)", lastStr)
		}
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
