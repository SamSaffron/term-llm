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
func (rt *serveRuntime) recordStatsEvent(e llm.Event) {
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	s := a.stats
	start := func(id string) {
		if a.tools[id] || a.ended[id] {
			return
		}
		if a.tools == nil {
			a.tools = map[string]bool{}
		}
		a.tools[id] = true
		s.ToolStart()
	}
	end := func(id string) {
		// Only an observed start can close a tool interval. Duplicate or
		// unmatched ends must not restart model timing or discard usage.
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
			start(e.Tool.ID)
		}
	case llm.EventToolExecStart:
		start(e.ToolCallID)
	case llm.EventToolExecEnd:
		end(e.ToolCallID)
	case llm.EventDiscoveryCall:
		if e.DiscoveryCall != nil {
			a.committed = true
			a.attempt = llm.Usage{}
			a.attemptCalls = 0
			start(e.DiscoveryCall.ID)
		}
	case llm.EventDiscoveryOutput:
		if e.DiscoveryOutput != nil {
			end(e.DiscoveryOutput.CallID)
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
	var ttft, gen time.Duration
	observed, tokens := 0, 0
	var cost float64
	priced, unpriced := 0, 0
	for _, c := range calls {
		if c.ObservedOutput && c.GenerationTime > 0 {
			observed++
			ttft += c.TTFT
			gen += c.GenerationTime
			tokens += c.OutputTokens
		}
		one := ui.NewSessionStats()
		one.AddSubagentUsageForModel(c.Model, c.InputTokens, c.OutputTokens, c.CachedInputTokens, c.CacheWriteTokens)
		if v, err := ui.EstimateSessionStatsCost(one, ""); err == nil {
			cost += v
			priced++
		} else {
			unpriced++
		}
	}
	if observed > 0 {
		out.TTFTMS = statsMS(ttft / time.Duration(observed))
		v := float64(tokens) / gen.Seconds()
		out.OutputTokensPerSecond = &v
	}
	if priced > 0 {
		out.CostUSD = &cost
	}
	out.CostPartial = unpriced > 0
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
		for _, c := range run.UsageCalls {
			r := group(c.Model)
			u := c.Usage
			r.InputTokens += u.InputTokens
			r.OutputTokens += u.OutputTokens
			r.CachedInputTokens += u.CachedInputTokens
			r.CacheWriteTokens += u.CacheWriteTokens
			r.LLMTurns++
			costs[r.Model].AddSubagentUsageForModel(c.Model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := groups[k]
		cs, _ := costs[k].UsageCalls()
		known, priced, missing := 0.0, 0, 0
		for _, c := range cs {
			one := ui.NewSessionStats()
			one.AddSubagentUsageForModel(c.Model, c.InputTokens, c.OutputTokens, c.CachedInputTokens, c.CacheWriteTokens)
			if v, err := ui.EstimateSessionStatsCost(one, ""); err == nil {
				known += v
				priced++
			} else {
				missing++
			}
		}
		if priced > 0 {
			r.CostUSD = &known
		}
		r.CostPartial = missing > 0
		out.Models = append(out.Models, *r)
	}
	for _, kind := range []string{"Private Side-Question Usage", "Guardian Usage", "Compaction Usage"} {
		var u llm.Usage
		n := 0
		for _, c := range calls {
			match := kind == "Private Side-Question Usage" && c.SideQuestion || kind == "Guardian Usage" && c.Guardian || kind == "Compaction Usage" && c.Compaction
			if match {
				n++
				u.Add(llm.Usage{InputTokens: c.InputTokens, OutputTokens: c.OutputTokens, CachedInputTokens: c.CachedInputTokens, CacheWriteTokens: c.CacheWriteTokens})
			}
		}
		out.Sections = append(out.Sections, statsUsageSection(kind, n, u))
	}
	out.Sections = append(out.Sections, statsUsageSection("Handover / Path-Note Usage", handoverCalls, handoverUsage))
	out.Sections = append(out.Sections, serveStatsSection{Title: "Runtime Activity", Rows: []serveStatsRow{statsRow("Retries", retries), statsRow("Running", running), statsRow("LLM calls", s.LLMCallCount), statsRow("Tool calls", s.ToolCallCount)}})
	return out
}
