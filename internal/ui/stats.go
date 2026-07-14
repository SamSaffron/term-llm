package ui

import (
	"fmt"
	"strings"
	"time"
)

// UsageCall is usage and performance data for one provider request made by this
// process. Keeping request boundaries is important because some pricing tiers
// apply to each request independently.
type UsageCall struct {
	Model             string
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	CacheWriteTokens  int
	TTFT              time.Duration
	GenerationTime    time.Duration
	ObservedOutput    bool
	Compaction        bool
}

// SessionStats tracks statistics for a session.
type SessionStats struct {
	StartTime         time.Time
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	CacheWriteTokens  int
	ToolCallCount     int
	LLMCallCount      int

	CompactionInputTokens       int
	CompactionOutputTokens      int
	CompactionCachedInputTokens int
	CompactionCacheWriteTokens  int
	CompactionLLMCallCount      int

	lastInputTokens  int
	lastOutputTokens int
	peakInputTokens  int
	hasPerCallUsage  bool

	LLMTime       time.Duration
	ToolTime      time.Duration
	lastEventTime time.Time
	inTool        bool

	currentModel       string
	requestStartTime   time.Time
	firstActivityTime  time.Time
	activityStartTime  time.Time
	activityDuration   time.Duration
	usageCalls         []UsageCall
	hasHistoricalUsage bool
	estimatedCostUSD   *float64
}

func NewSessionStats() *SessionStats {
	now := time.Now()
	return &SessionStats{StartTime: now, lastEventTime: now}
}

// SeedTotals initializes cumulative counters from persisted session metrics.
// Process-local call, timing, model, and cost state is reset: persisted totals
// do not contain enough request detail to price or calculate throughput safely.
func (s *SessionStats) SeedTotals(input, output, cached, cacheWrite, toolCalls, llmCalls int) {
	s.InputTokens, s.OutputTokens = input, output
	s.CachedInputTokens, s.CacheWriteTokens = cached, cacheWrite
	s.ToolCallCount, s.LLMCallCount = toolCalls, llmCalls
	s.CompactionInputTokens, s.CompactionOutputTokens = 0, 0
	s.CompactionCachedInputTokens, s.CompactionCacheWriteTokens = 0, 0
	s.CompactionLLMCallCount = 0
	s.lastInputTokens, s.lastOutputTokens, s.peakInputTokens = 0, 0, 0
	s.hasPerCallUsage = false
	s.currentModel = ""
	s.requestStartTime, s.firstActivityTime, s.activityStartTime = time.Time{}, time.Time{}, time.Time{}
	s.activityDuration = 0
	s.usageCalls = nil
	s.hasHistoricalUsage = input != 0 || output != 0 || cached != 0 || cacheWrite != 0 || llmCalls != 0
	s.estimatedCostUSD = nil
}

// SetModel sets the model attached to subsequently completed usage calls.
func (s *SessionStats) SetModel(model string) { s.currentModel = strings.TrimSpace(model) }

func (s *SessionStats) AddUsage(input, output, cached, cacheWrite int) {
	s.addUsageAt(input, output, cached, cacheWrite, time.Now(), true)
}

func (s *SessionStats) addUsageAt(input, output, cached, cacheWrite int, now time.Time, recordPerformance bool) {
	s.stopActivityAt(now)
	if input == 0 && output == 0 && cached == 0 && cacheWrite == 0 {
		// Some providers emit a terminal usage event with no counters. It still
		// completes the pending request timing, but is not a meaningful usage call.
		s.resetPendingCall()
		return
	}
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
	s.CacheWriteTokens += cacheWrite
	s.LLMCallCount++
	totalContext := input + cached + output
	s.lastInputTokens, s.lastOutputTokens, s.hasPerCallUsage = totalContext, output, true
	if totalContext > s.peakInputTokens {
		s.peakInputTokens = totalContext
	}

	call := UsageCall{Model: s.currentModel, InputTokens: input, OutputTokens: output, CachedInputTokens: cached, CacheWriteTokens: cacheWrite}
	if recordPerformance && s.activityDuration > 0 {
		call.ObservedOutput = true
		call.GenerationTime = s.activityDuration
		if !s.requestStartTime.IsZero() && !s.firstActivityTime.IsZero() && !s.firstActivityTime.Before(s.requestStartTime) {
			call.TTFT = s.firstActivityTime.Sub(s.requestStartTime)
		}
	}
	s.usageCalls = append(s.usageCalls, call)
	s.resetPendingCall()
}

// AddCompactionUsage records a helper compaction call against the current model.
// Unlike AddUsage, it deliberately leaves pending main-call timing untouched.
func (s *SessionStats) AddCompactionUsage(input, output, cached, cacheWrite int) {
	s.AddCompactionUsageForModel(s.currentModel, input, output, cached, cacheWrite)
}

// AddCompactionUsageForModel records compaction usage against the model that
// performed it without changing the model or timing of a pending main call.
func (s *SessionStats) AddCompactionUsageForModel(model string, input, output, cached, cacheWrite int) {
	if input == 0 && output == 0 && cached == 0 && cacheWrite == 0 {
		return
	}
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
	s.CacheWriteTokens += cacheWrite
	s.LLMCallCount++
	totalContext := input + cached + output
	s.lastInputTokens, s.lastOutputTokens, s.hasPerCallUsage = totalContext, output, true
	if totalContext > s.peakInputTokens {
		s.peakInputTokens = totalContext
	}
	s.usageCalls = append(s.usageCalls, UsageCall{
		Model:             strings.TrimSpace(model),
		InputTokens:       input,
		OutputTokens:      output,
		CachedInputTokens: cached,
		CacheWriteTokens:  cacheWrite,
		Compaction:        true,
	})
	s.CompactionInputTokens += input
	s.CompactionOutputTokens += output
	s.CompactionCachedInputTokens += cached
	s.CompactionCacheWriteTokens += cacheWrite
	s.CompactionLLMCallCount++
}

// DiscardUsage removes provisional usage calls from the tail and resets any
// uncompleted attempt activity. Performance is always derived from retained
// call records, so TTFT and throughput are restored exactly after a retry.
func (s *SessionStats) DiscardUsage(input, output, cached, cacheWrite, calls int) {
	s.InputTokens = max(0, s.InputTokens-input)
	s.OutputTokens = max(0, s.OutputTokens-output)
	s.CachedInputTokens = max(0, s.CachedInputTokens-cached)
	s.CacheWriteTokens = max(0, s.CacheWriteTokens-cacheWrite)
	s.LLMCallCount = max(0, s.LLMCallCount-calls)
	remaining := calls
	for i := len(s.usageCalls) - 1; i >= 0 && remaining > 0; i-- {
		if s.usageCalls[i].Compaction {
			continue
		}
		s.usageCalls = append(s.usageCalls[:i], s.usageCalls[i+1:]...)
		remaining--
	}
	s.rebuildPerCallHints()
	s.resetPendingCall()
	s.estimatedCostUSD = nil
}

func (s *SessionStats) rebuildPerCallHints() {
	s.lastInputTokens, s.lastOutputTokens, s.peakInputTokens = 0, 0, 0
	s.hasPerCallUsage = false
	for _, call := range s.usageCalls {
		total := call.InputTokens + call.CachedInputTokens + call.OutputTokens
		s.lastInputTokens, s.lastOutputTokens, s.hasPerCallUsage = total, call.OutputTokens, true
		if total > s.peakInputTokens {
			s.peakInputTokens = total
		}
	}
}

func (s *SessionStats) RequestStart() { s.requestStartAt(time.Now()) }
func (s *SessionStats) requestStartAt(now time.Time) {
	s.stopActivityAt(now)
	s.resetPendingCall()
	s.requestStartTime = now
}

// ScheduleRetryStart records when a retried provider request will start. Retry
// events are emitted before the provider wait, so TTFT must exclude that delay.
func (s *SessionStats) ScheduleRetryStart(waitSecs float64) {
	if waitSecs < 0 {
		waitSecs = 0
	}
	s.scheduleRetryStartAt(time.Now(), time.Duration(waitSecs*float64(time.Second)))
}

func (s *SessionStats) scheduleRetryStartAt(now time.Time, wait time.Duration) {
	s.stopActivityAt(now)
	s.resetPendingCall()
	s.requestStartTime = now.Add(wait)
}

// ObserveOutput records generation activity. This includes visible text and
// reasoning as well as hidden/encrypted reasoning events, but not tool calls.
func (s *SessionStats) ObserveOutput() { s.outputAt(time.Now()) }
func (s *SessionStats) outputAt(now time.Time) {
	if s.firstActivityTime.IsZero() {
		s.firstActivityTime = now
	}
	if s.activityStartTime.IsZero() {
		s.activityStartTime = now
	}
}

// GenerationEnd closes the current activity interval. AddUsage associates the
// accumulated interval with that completed provider request.
func (s *SessionStats) GenerationEnd() { s.stopActivityAt(time.Now()) }
func (s *SessionStats) stopActivityAt(now time.Time) {
	if !s.activityStartTime.IsZero() && !now.Before(s.activityStartTime) {
		s.activityDuration += now.Sub(s.activityStartTime)
	}
	s.activityStartTime = time.Time{}
}
func (s *SessionStats) resetPendingCall() {
	s.requestStartTime, s.firstActivityTime, s.activityStartTime = time.Time{}, time.Time{}, time.Time{}
	s.activityDuration = 0
}

// UsageCalls returns current-process calls and whether they represent the whole
// displayed session. A false completeness value means historical seeded usage
// prevents a truthful whole-session cost.
func (s *SessionStats) UsageCalls() ([]UsageCall, bool) {
	return append([]UsageCall(nil), s.usageCalls...), !s.hasHistoricalUsage
}

func (s *SessionStats) SetEstimatedCost(cost float64) {
	if cost >= 0 {
		s.estimatedCostUSD = &cost
	}
}
func (s *SessionStats) ClearEstimatedCost() { s.estimatedCostUSD = nil }

func (s *SessionStats) ToolStart() {
	now := time.Now()
	s.stopActivityAt(now)
	if !s.inTool {
		s.LLMTime += now.Sub(s.lastEventTime)
	}
	s.lastEventTime, s.inTool = now, true
	s.ToolCallCount++
}
func (s *SessionStats) ToolEnd() {
	now := time.Now()
	if s.inTool {
		s.ToolTime += now.Sub(s.lastEventTime)
	}
	s.lastEventTime, s.inTool = now, false
	s.requestStartTime = now
	s.firstActivityTime = time.Time{}
	s.activityDuration = 0
}
func (s *SessionStats) Finalize() {
	now := time.Now()
	s.stopActivityAt(now)
	if s.inTool {
		s.ToolTime += now.Sub(s.lastEventTime)
	} else {
		s.LLMTime += now.Sub(s.lastEventTime)
	}
	s.lastEventTime = now
}

func (s SessionStats) Render() string {
	parts := []string{fmt.Sprintf("%.1fs", time.Since(s.StartTime).Seconds())}
	tokenParts := []string{fmt.Sprintf("%s in", formatStatsTokenCount(s.InputTokens))}
	if s.CachedInputTokens > 0 {
		tokenParts = append(tokenParts, fmt.Sprintf("%s cached", formatStatsTokenCount(s.CachedInputTokens)))
	}
	if s.CacheWriteTokens > 0 {
		tokenParts = append(tokenParts, fmt.Sprintf("%s cache write", formatStatsTokenCount(s.CacheWriteTokens)))
	}
	parts = append(parts, fmt.Sprintf("%s → %s out", strings.Join(tokenParts, " + "), formatStatsTokenCount(s.OutputTokens)))

	var firstTTFT time.Duration
	var generated int
	var generationTime time.Duration
	for _, call := range s.usageCalls {
		if !call.ObservedOutput {
			continue
		}
		if firstTTFT == 0 && call.TTFT > 0 {
			firstTTFT = call.TTFT
		}
		generated += call.OutputTokens
		generationTime += call.GenerationTime
	}
	performance := []string{}
	if firstTTFT > 0 {
		performance = append(performance, fmt.Sprintf("TTFT %.1fs", firstTTFT.Seconds()))
	}
	if generated > 0 && generationTime > 0 {
		performance = append(performance, fmt.Sprintf("%.0f tok/s", float64(generated)/generationTime.Seconds()))
	}
	if len(performance) > 0 {
		parts = append(parts, strings.Join(performance, ", "))
	}
	if s.estimatedCostUSD != nil {
		parts = append(parts, fmt.Sprintf("$%.4f", *s.estimatedCostUSD))
	}
	activity := []string{}
	if s.ToolCallCount > 0 {
		activity = append(activity, fmt.Sprintf("%d %s", s.ToolCallCount, plural(s.ToolCallCount, "tool", "tools")))
	}
	if s.LLMCallCount > 0 {
		activity = append(activity, fmt.Sprintf("%d %s", s.LLMCallCount, plural(s.LLMCallCount, "call", "calls")))
	}
	if len(activity) > 0 {
		parts = append(parts, strings.Join(activity, ", "))
	}
	return "Stats: " + strings.Join(parts, " | ")
}

func formatStatsTokenCount(n int) string { return strings.Replace(FormatTokenCount(n), "k", "K", 1) }
func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
