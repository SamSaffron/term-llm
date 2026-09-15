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
	Handover          bool
	SideQuestion      bool
	Guardian          bool
	Subagent          bool
}

// Kind names why the request was billed. The values deliberately match the
// session.ModelUsageKind constants so observed and recorded breakdowns label
// the same spend identically. The flags are mutually exclusive by
// construction; an unflagged call is the session's own turn.
func (c UsageCall) Kind() string {
	switch {
	case c.Compaction:
		return "compaction"
	case c.Handover:
		return "handover"
	case c.SideQuestion:
		return "side_question"
	case c.Guardian:
		return "guardian"
	case c.Subagent:
		return "subagent"
	default:
		return "main"
	}
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

	GuardianInputTokens       int
	GuardianOutputTokens      int
	GuardianCachedInputTokens int
	GuardianCacheWriteTokens  int
	GuardianLLMCallCount      int

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
	s.GuardianInputTokens, s.GuardianOutputTokens = 0, 0
	s.GuardianCachedInputTokens, s.GuardianCacheWriteTokens = 0, 0
	s.GuardianLLMCallCount = 0
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

// Model reports the model that completed usage calls are billed to. It follows
// the request and any mid-run model switch, which is what durable attribution
// has to record: the session's configured model is not necessarily the one that
// ran the turn.
func (s *SessionStats) Model() string { return s.currentModel }

func (s *SessionStats) AddUsage(input, output, cached, cacheWrite int) {
	s.addUsageAt(input, output, cached, cacheWrite, time.Now(), true)
}

func (s *SessionStats) addUsageAt(input, output, cached, cacheWrite int, now time.Time, recordPerformance bool) {
	s.stopActivityAt(now)
	s.accrueActiveTimeAt(now)
	s.lastEventTime = now
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

// AddHandoverUsageForModel records handover-helper usage without disturbing
// main-request timing or treating it as context compaction.
func (s *SessionStats) AddHandoverUsageForModel(model string, input, output, cached, cacheWrite int) {
	if input == 0 && output == 0 && cached == 0 && cacheWrite == 0 {
		return
	}
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
	s.CacheWriteTokens += cacheWrite
	s.LLMCallCount++
	s.usageCalls = append(s.usageCalls, UsageCall{
		Model:             strings.TrimSpace(model),
		InputTokens:       input,
		OutputTokens:      output,
		CachedInputTokens: cached,
		CacheWriteTokens:  cacheWrite,
		Handover:          true,
	})
}

// AddSideQuestionUsageForModel records side-question usage in aggregate token,
// call, and pricing totals without disturbing main-request timing or context hints.
func (s *SessionStats) AddSideQuestionUsageForModel(model string, input, output, cached, cacheWrite int) {
	if input == 0 && output == 0 && cached == 0 && cacheWrite == 0 {
		return
	}
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
	s.CacheWriteTokens += cacheWrite
	s.LLMCallCount++
	s.usageCalls = append(s.usageCalls, UsageCall{
		Model:             strings.TrimSpace(model),
		InputTokens:       input,
		OutputTokens:      output,
		CachedInputTokens: cached,
		CacheWriteTokens:  cacheWrite,
		SideQuestion:      true,
	})
}

// AddGuardianUsageForModel records guardian-review usage in aggregate token,
// call, and pricing totals without disturbing main-request timing or context hints.
func (s *SessionStats) AddGuardianUsageForModel(model string, input, output, cached, cacheWrite int) {
	if input == 0 && output == 0 && cached == 0 && cacheWrite == 0 {
		return
	}
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
	s.CacheWriteTokens += cacheWrite
	s.LLMCallCount++
	s.GuardianInputTokens += input
	s.GuardianOutputTokens += output
	s.GuardianCachedInputTokens += cached
	s.GuardianCacheWriteTokens += cacheWrite
	s.GuardianLLMCallCount++
	s.usageCalls = append(s.usageCalls, UsageCall{
		Model:             strings.TrimSpace(model),
		InputTokens:       input,
		OutputTokens:      output,
		CachedInputTokens: cached,
		CacheWriteTokens:  cacheWrite,
		Guardian:          true,
	})
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
		// Helper categories are billed independently of the main attempt, so a
		// rollback of that attempt must never consume them. Handover belongs
		// here too: it is real spend that outlives the attempt it ran beside.
		call := s.usageCalls[i]
		if call.Compaction || call.SideQuestion || call.Guardian || call.Subagent || call.Handover {
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
		if call.SideQuestion || call.Guardian || call.Subagent || call.Handover {
			continue
		}
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
	s.lastEventTime = now
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
	s.accrueActiveTimeAt(now)
	s.resetPendingCall()
	s.requestStartTime = now.Add(wait)
	s.lastEventTime = s.requestStartTime
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
	s.accrueActiveTimeAt(now)
	s.lastEventTime, s.inTool = now, true
	s.ToolCallCount++
}
func (s *SessionStats) ToolEnd() {
	now := time.Now()
	s.accrueActiveTimeAt(now)
	s.lastEventTime, s.inTool = now, false
	s.requestStartTime = now
	s.firstActivityTime = time.Time{}
	s.activityDuration = 0
}
func (s *SessionStats) Finalize() {
	now := time.Now()
	s.stopActivityAt(now)
	s.accrueActiveTimeAt(now)
	s.lastEventTime = now
	s.resetPendingCall()
	s.inTool = false
}

// SnapshotAt returns a non-mutating view with the current active interval accrued.
func (s SessionStats) SnapshotAt(now time.Time) SessionStats {
	s.stopActivityAt(now)
	s.accrueActiveTimeAt(now)
	s.lastEventTime = now
	return s
}

func (s *SessionStats) accrueActiveTimeAt(now time.Time) {
	if s.lastEventTime.IsZero() || now.Before(s.lastEventTime) {
		return
	}
	if s.inTool {
		s.ToolTime += now.Sub(s.lastEventTime)
		return
	}
	if s.requestStartTime.IsZero() || now.Before(s.requestStartTime) {
		return
	}
	start := s.lastEventTime
	if start.Before(s.requestStartTime) {
		start = s.requestStartTime
	}
	s.LLMTime += now.Sub(start)
}

func (s SessionStats) Render() string {
	active := s.LLMTime + s.ToolTime
	parts := []string{"active " + FormatStatsDuration(active)}
	if s.ToolTime > 0 {
		parts = append(parts, "model "+FormatStatsDuration(s.LLMTime)+" + tools "+FormatStatsDuration(s.ToolTime))
	}
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
		performance = append(performance, "TTFT "+FormatStatsDuration(firstTTFT))
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

// FormatStatsDuration renders elapsed work time. Seconds stop being readable
// after a minute or two, so anything longer uses the compact elapsed format
// already used for progress; below a minute the tenth still matters, which that
// format cannot express (a 0.4s time to first token would read as 0s).
func FormatStatsDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return FormatElapsedDuration(d)
}
func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// AddSubagentUsageForModel includes delegated usage without changing parent context or timing.
func (s *SessionStats) AddSubagentUsageForModel(model string, input, output, cached, cacheWrite int) {
	if input == 0 && output == 0 && cached == 0 && cacheWrite == 0 {
		return
	}
	s.InputTokens += input
	s.OutputTokens += output
	s.CachedInputTokens += cached
	s.CacheWriteTokens += cacheWrite
	s.LLMCallCount++
	s.usageCalls = append(s.usageCalls, UsageCall{Model: strings.TrimSpace(model), InputTokens: input, OutputTokens: output, CachedInputTokens: cached, CacheWriteTokens: cacheWrite, Subagent: true})
}
