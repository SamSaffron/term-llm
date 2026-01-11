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
	TurnCount         int // For multi-turn sessions (chat)

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

// AddUsage adds token usage to the stats.
func (s *SessionStats) AddUsage(input, output, cached int) {
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
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

// AddTurn increments the turn count.
func (s *SessionStats) AddTurn() {
	s.TurnCount++
}

// Render returns the stats as a compact single-line string.
func (s SessionStats) Render() string {
	total := time.Since(s.StartTime)

	// Format tokens with optional cache info
	var tokensStr string
	if s.CachedInputTokens > 0 {
		tokensStr = fmt.Sprintf("%s in (%s cached) / %s out",
			formatTokenCount(s.InputTokens),
			formatTokenCount(s.CachedInputTokens),
			formatTokenCount(s.OutputTokens))
	} else {
		tokensStr = fmt.Sprintf("%s in / %s out",
			formatTokenCount(s.InputTokens),
			formatTokenCount(s.OutputTokens))
	}

	// Format time breakdown
	var timeStr string
	if s.ToolCallCount > 0 {
		timeStr = fmt.Sprintf("%.1fs (llm %.1fs + tool %.1fs)",
			total.Seconds(), s.LLMTime.Seconds(), s.ToolTime.Seconds())
	} else {
		timeStr = fmt.Sprintf("%.1fs", total.Seconds())
	}

	if s.TurnCount > 0 {
		// Multi-turn format: Stats: 34.5s | 3 turns | 1.2k in / 4.5k out | 5 tools
		return fmt.Sprintf("Stats: %s | %d turns | %s | %d tools",
			timeStr, s.TurnCount, tokensStr, s.ToolCallCount)
	}

	return fmt.Sprintf("Stats: %s | %s | %d tools",
		timeStr, tokensStr, s.ToolCallCount)
}
