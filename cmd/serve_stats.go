package cmd

import (
	"sort"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// serveStats is independent of the run lock. Never seed it from durable totals:
// those totals have neither request boundaries nor historical model identities.
type serveStats struct {
	childrenStarted map[string]bool
	mu              sync.Mutex
	stats           *ui.SessionStats
	children        *ui.SubagentTracker
	running         bool
	tools           map[string]bool
	ended           map[string]bool
	attempt         llm.Usage
	attemptCalls    int
	committed       bool
	retries         int
	handoverUsage   llm.Usage
	handoverCalls   int
}

func (a *serveStats) init() {
	if a.stats == nil {
		a.stats = ui.NewSessionStats()
		a.stats.Finalize()
	}
	if a.children == nil {
		a.children = ui.NewSubagentTracker()
		a.childrenStarted = make(map[string]bool)
	}
}
func (rt *serveRuntime) startStats(model string) {
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	a.stats.SetModel(model)
	a.stats.RequestStart()
	a.running = true
	a.tools = map[string]bool{}
	a.ended = map[string]bool{}
	a.attempt = llm.Usage{}
	a.attemptCalls = 0
	a.committed = false
}
func (rt *serveRuntime) finishStats() {
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stats != nil {
		a.stats.Finalize()
	}
	a.running = false
}
func (a *serveStats) startTool(s *ui.SessionStats, id string) {
	if a.tools[id] || a.ended[id] {
		return
	}
	if a.tools == nil {
		a.tools = map[string]bool{}
	}
	a.tools[id] = true
	s.ToolStart()
}

func (a *serveStats) endTool(s *ui.SessionStats, id string) {
	// Only an observed start can close a tool interval. Duplicate or unmatched
	// ends must not restart model timing or discard usage.
	if !a.tools[id] {
		return
	}
	if a.ended == nil {
		a.ended = map[string]bool{}
	}
	a.ended[id] = true
	delete(a.tools, id)
	if len(a.tools) == 0 {
		s.ToolEnd()
	}
	a.attempt = llm.Usage{}
	a.attemptCalls = 0
	a.committed = false
}

func (a *serveStats) discardAttempt(s *ui.SessionStats) {
	// Keep time spent on a failed attempt, even when it produced no usage.
	// DiscardUsage resets pending timing, so accrue it first. Copying the
	// snapshot back also preserves an overlapping native-tool interval.
	*s = s.SnapshotAt(time.Now())
	u := a.attempt
	s.DiscardUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens, a.attemptCalls)
	if len(a.tools) == 0 {
		// Some provider fallbacks discard without an EventRetry. Start the
		// replacement attempt now; a subsequent retry reschedules past backoff.
		s.RequestStart()
	}
	a.attempt = llm.Usage{}
	a.attemptCalls = 0
	a.committed = false
}

func (rt *serveRuntime) recordStatsEvent(e llm.Event) {
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	s := a.stats
	switch e.Type {
	case llm.EventTextDelta, llm.EventReasoningDelta:
		a.committed = false
		if e.Text != "" || llm.IsEncryptedReasoningDelta(e) {
			s.ObserveOutput()
		}
	case llm.EventModelSwitch:
		s.SetModel(e.Model)
	case llm.EventToolCall:
		a.committed = true
		a.attempt = llm.Usage{}
		a.attemptCalls = 0
		if e.Tool != nil {
			a.startTool(s, e.Tool.ID)
		}
	case llm.EventToolExecStart:
		a.startTool(s, e.ToolCallID)
	case llm.EventToolExecEnd:
		a.endTool(s, e.ToolCallID)
	case llm.EventDiscoveryCall:
		if e.DiscoveryCall != nil {
			a.committed = true
			a.attempt = llm.Usage{}
			a.attemptCalls = 0
			a.startTool(s, e.DiscoveryCall.ID)
		}
	case llm.EventDiscoveryOutput:
		if e.DiscoveryOutput != nil {
			a.endTool(s, e.DiscoveryOutput.CallID)
		}
	case llm.EventUsage:
		s.GenerationEnd()
		if e.Use != nil {
			u := *e.Use
			s.AddUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
			if !a.committed && !u.BillableCountersZero() {
				a.attempt.Add(u)
				a.attemptCalls++
			}
		}
	case llm.EventRetry:
		a.retries++
		s.ScheduleRetryStart(e.RetryWaitSecs)
	case llm.EventAttemptDiscard:
		a.discardAttempt(s)
	case llm.EventDone, llm.EventError:
		s.Finalize()
		a.running = false
	}
}
func (rt *serveRuntime) recordHelperStats(kind, model string, u llm.Usage) {
	if u.BillableCountersZero() {
		return
	}
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	switch kind {
	case "guardian":
		a.stats.AddGuardianUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	case "compaction":
		a.stats.AddCompactionUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	case "side_question":
		a.stats.AddSideQuestionUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	case "handover", "path_note":
		// SessionStats.DiscardUsage does not protect Handover calls. Use the
		// helper-safe ledger category and retain the display category here so
		// a concurrent main-attempt rollback cannot erase billed helper work.
		a.stats.AddSubagentUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
		a.handoverUsage.Add(u)
		a.handoverCalls++
	case "subagent":
		a.stats.AddSubagentUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	}
}
func (rt *serveRuntime) recordSubagentStats(id string, e tools.SubagentEvent) {
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	t := a.children
	t.GetOrCreate(id, "")
	switch e.Type {
	case tools.SubagentEventInit:
		a.childrenStarted[id] = true
		t.HandleInitAt(id, e.Provider, e.Model, e.Timestamp)
	case tools.SubagentEventToolStart:
		t.HandleToolStartAt(id, e.ToolCallID, e.ToolName, e.ToolInfo, e.ToolArgs, e.Timestamp)
	case tools.SubagentEventToolEnd:
		t.HandleToolEndAt(id, e.ToolCallID, e.ToolName, e.Success, e.Timestamp)
	case tools.SubagentEventDone:
		t.MarkDoneAt(id, e.Timestamp)
	case tools.SubagentEventUsage:
		t.HandleUsageEvent(id, e)
		model := e.Model
		if model == "" {
			model = t.ResolvedModel(id)
		}
		a.stats.AddSubagentUsageForModel(model, e.InputTokens, e.OutputTokens, e.CachedInputTokens, e.CacheWriteTokens)
	case tools.SubagentEventGuardian:
		if e.Guardian != nil {
			t.HandleGuardianEvent(id, *e.Guardian)
			g := e.Guardian
			u := g.Usage
			a.stats.AddGuardianUsageForModel(g.Model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
		}
	}
}

func statsMetrics(s *ui.SessionStats) webSessionMetrics {
	return webSessionMetrics{InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, CachedInputTokens: s.CachedInputTokens, CacheWriteTokens: s.CacheWriteTokens, ToolCalls: s.ToolCallCount, LLMTurns: s.LLMCallCount}
}
func statsMS(d time.Duration) *int64 { n := d.Milliseconds(); return &n }
func appendMainCallStats(out *serveStatsResponse, calls []ui.UsageCall) {
	var ttft, generation time.Duration
	observed, tokens := 0, 0
	var cost float64
	priced, unpriced := 0, 0
	for _, call := range calls {
		if call.ObservedOutput && call.GenerationTime > 0 {
			observed++
			ttft += call.TTFT
			generation += call.GenerationTime
			tokens += call.OutputTokens
		}
		one := ui.NewSessionStats()
		one.AddSubagentUsageForModel(call.Model, call.InputTokens, call.OutputTokens, call.CachedInputTokens, call.CacheWriteTokens)
		if value, err := ui.EstimateSessionStatsCost(one, ""); err == nil {
			cost += value
			priced++
		} else {
			unpriced++
		}
	}
	if observed > 0 {
		out.TTFTMS = statsMS(ttft / time.Duration(observed))
		value := float64(tokens) / generation.Seconds()
		out.OutputTokensPerSecond = &value
	}
	if priced > 0 {
		out.CostUSD = &cost
	}
	out.CostPartial = unpriced > 0
}

func appendSubagentModelStats(out *serveStatsResponse, runs []ui.SubagentProgress, started map[string]bool, now time.Time) {
	groups := map[string]*serveStatsModel{}
	costs := map[string]*ui.SessionStats{}
	group := func(model string) *serveStatsModel {
		if model == "" {
			model = "unknown"
		}
		if groups[model] == nil {
			groups[model] = &serveStatsModel{Model: model}
			costs[model] = ui.NewSessionStats()
		}
		return groups[model]
	}
	for _, run := range runs {
		// Usage alone does not establish a run start time. Do not manufacture
		// timing for callbacks whose initialization was not observed.
		if started[run.ToolCallID] || run.ToolCalls > 0 {
			row := group(run.ResolvedModel)
			row.Running = row.Running || !run.Done
			row.ToolCalls += run.ToolCalls
			if started[run.ToolCallID] {
				elapsed, toolTime := run.Timing(now)
				if row.ActiveMS == nil {
					row.ActiveMS = statsMS(0)
					row.ToolMS = statsMS(0)
				}
				*row.ActiveMS += elapsed.Milliseconds()
				*row.ToolMS += toolTime.Milliseconds()
			}
		}
		for _, call := range run.UsageCalls {
			row := group(call.Model)
			u := call.Usage
			row.InputTokens += u.InputTokens
			row.OutputTokens += u.OutputTokens
			row.CachedInputTokens += u.CachedInputTokens
			row.CacheWriteTokens += u.CacheWriteTokens
			row.LLMTurns++
			costs[row.Model].AddSubagentUsageForModel(call.Model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		row := groups[key]
		calls, _ := costs[key].UsageCalls()
		known, priced, missing := 0.0, 0, 0
		for _, call := range calls {
			one := ui.NewSessionStats()
			one.AddSubagentUsageForModel(call.Model, call.InputTokens, call.OutputTokens, call.CachedInputTokens, call.CacheWriteTokens)
			if value, err := ui.EstimateSessionStatsCost(one, ""); err == nil {
				known += value
				priced++
			} else {
				missing++
			}
		}
		if priced > 0 {
			row.CostUSD = &known
		}
		row.CostPartial = missing > 0
		out.Models = append(out.Models, *row)
	}
}

func appendHelperUsageSections(out *serveStatsResponse, calls []ui.UsageCall) {
	for _, kind := range []string{"Private Side-Question Usage", "Guardian Usage", "Compaction Usage"} {
		var usage llm.Usage
		count := 0
		for _, call := range calls {
			match := kind == "Private Side-Question Usage" && call.SideQuestion || kind == "Guardian Usage" && call.Guardian || kind == "Compaction Usage" && call.Compaction
			if match {
				count++
				usage.Add(llm.Usage{InputTokens: call.InputTokens, OutputTokens: call.OutputTokens, CachedInputTokens: call.CachedInputTokens, CacheWriteTokens: call.CacheWriteTokens})
			}
		}
		out.Sections = append(out.Sections, statsUsageSection(kind, count, usage))
	}
}

func (rt *serveRuntime) statsSnapshot() *serveStatsResponse {
	a := &rt.stats
	a.mu.Lock()
	if a.stats == nil {
		a.mu.Unlock()
		return nil
	}
	now := time.Now()
	s := a.stats.SnapshotAt(now)
	calls, _ := s.UsageCalls()
	runs := a.children.Snapshots()
	started := make(map[string]bool, len(a.childrenStarted))
	for id, observed := range a.childrenStarted {
		started[id] = observed
	}
	retries, running := a.retries, a.running
	handoverUsage, handoverCalls := a.handoverUsage, a.handoverCalls
	a.mu.Unlock()
	out := &serveStatsResponse{Metrics: statsMetrics(&s), Scope: "runtime_local", Models: []serveStatsModel{}, Sections: []serveStatsSection{}, ActiveMS: statsMS(s.LLMTime + s.ToolTime), ModelMS: statsMS(s.LLMTime), ToolMS: statsMS(s.ToolTime)}
	appendMainCallStats(out, calls)
	appendSubagentModelStats(out, runs, started, now)
	appendHelperUsageSections(out, calls)
	out.Sections = append(out.Sections, statsUsageSection("Handover / Path-Note Usage", handoverCalls, handoverUsage))
	out.Sections = append(out.Sections, serveStatsSection{Title: "Runtime Activity", Rows: []serveStatsRow{statsRow("Retries", retries), statsRow("Running", running), statsRow("LLM calls", s.LLMCallCount), statsRow("Tool calls", s.ToolCallCount)}})
	return out
}
