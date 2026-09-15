package cmd

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// usageModel names the billing model for durable attribution.
//
// The model the turn actually ran on wins: a request may carry its own model and
// a run may switch models part way through, and recording the session's
// configured model instead would price the wrong rates and disagree with the
// observed per-model breakdown.
func (rt *serveRuntime) usageModel() string {
	if model := rt.observedUsageModel(); model != "" {
		return model
	}
	if rt.sessionMeta != nil {
		if model := strings.TrimSpace(rt.sessionMeta.Model); model != "" {
			return model
		}
	}
	return strings.TrimSpace(rt.defaultModel)
}

// observedUsageModel is the model the runtime last billed a request to.
func (rt *serveRuntime) observedUsageModel() string {
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stats == nil {
		return ""
	}
	return strings.TrimSpace(a.stats.Model())
}

// recordDurableModelUsage attributes a share of the session's durable totals to
// the model that spent it. Call it only alongside UpdateMetrics: the per-model
// rows are meant to decompose the aggregate session bucket, so recording work
// that never entered that bucket would make the two disagree.
func (rt *serveRuntime) recordDurableModelUsage(ctx context.Context, sessionID, model string, kind session.ModelUsageKind, u llm.Usage, llmTurns, toolCalls int) {
	rt.recordDurableModelUsageTimed(ctx, sessionID, model, kind, u, llmTurns, toolCalls, modelUsageTiming{})
}

// modelUsageTiming is the model and tool time one attribution spent, measured
// against the runtime clock it was read from.
type modelUsageTiming struct {
	LLMMS, ToolMS     int64
	llmMark, toolMark time.Duration
	measured          bool
}

// recordDurableModelUsageTimed reports whether the attribution reached the
// store. Callers hold the turn's clock until it does: a slice of time that is
// consumed but never written cannot be recovered from anywhere.
func (rt *serveRuntime) recordDurableModelUsageTimed(ctx context.Context, sessionID, model string, kind session.ModelUsageKind, u llm.Usage, llmTurns, toolCalls int, timing modelUsageTiming) bool {
	entry := session.ModelUsage{
		Model:             model,
		Kind:              kind,
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CachedInputTokens,
		CacheWriteTokens:  u.CacheWriteTokens,
		LLMTurns:          llmTurns,
		ToolCalls:         toolCalls,
		LLMMS:             timing.LLMMS,
		ToolMS:            timing.ToolMS,
	}
	// A turn that reported no tokens but spent time still belongs to a model:
	// dropping it would strand that slice of the clock on the next turn, which
	// may be a different model entirely.
	if rt.store == nil || strings.TrimSpace(sessionID) == "" || entry.RecordsNoWork() {
		return false
	}
	rt.beginTurnTimingWrite(timing)
	if err := rt.store.RecordModelUsage(ctx, sessionID, entry); err != nil {
		rt.abortTurnTiming(timing)
		log.Printf("[serve] RecordModelUsage failed for %s: %v", sessionID, err)
		return false
	}
	return true
}

// pendingTurnTiming reports the model and tool time accrued since the last
// committed turn without consuming it. A turn that is never written (no
// billable counters, a failed store call) must leave the clock where it was, so
// the time accrues to the next turn that does reach the store instead of
// vanishing.
//
// Helper calls made inside a turn (guardian, compaction, side questions) share
// the same clock, so their time lands on the turn's model rather than on the
// helper's own row.
func (rt *serveRuntime) pendingTurnTiming() modelUsageTiming {
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stats == nil {
		return modelUsageTiming{}
	}
	snapshot := a.stats.SnapshotAt(time.Now())
	// Time already handed to a write in progress is not pending: a reader that
	// sees the stored row and this remainder would otherwise count it twice.
	llmFrom := max(a.persistedLLMTime, a.writingLLMTime)
	toolFrom := max(a.persistedToolTime, a.writingToolTime)
	return modelUsageTiming{
		LLMMS:    max(0, (snapshot.LLMTime - llmFrom).Milliseconds()),
		ToolMS:   max(0, (snapshot.ToolTime - toolFrom).Milliseconds()),
		llmMark:  snapshot.LLMTime,
		toolMark: snapshot.ToolTime,
		measured: true,
	}
}

// beginTurnTimingWrite claims the measured slice for a write in flight, so a
// concurrent reader stops counting it as pending the moment the row can appear
// in the store. Under-reporting for the length of one write is recoverable;
// double counting a turn is not.
func (rt *serveRuntime) beginTurnTimingWrite(timing modelUsageTiming) {
	if !timing.measured {
		return
	}
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writingLLMTime = max(a.writingLLMTime, timing.llmMark)
	a.writingToolTime = max(a.writingToolTime, timing.toolMark)
}

// commitTurnTiming advances the persisted watermark to the mark the timing was
// measured at. The watermark only ever moves forward: a rewound clock (a
// discarded attempt) must not make the next turn re-record time already stored.
func (rt *serveRuntime) commitTurnTiming(timing modelUsageTiming) {
	if !timing.measured {
		return
	}
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	a.persistedLLMTime = max(a.persistedLLMTime, timing.llmMark)
	a.persistedToolTime = max(a.persistedToolTime, timing.toolMark)
}

// abortTurnTiming releases a claim whose write never landed, returning the time
// to the next turn that does reach the store.
func (rt *serveRuntime) abortTurnTiming(timing modelUsageTiming) {
	if !timing.measured {
		return
	}
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writingLLMTime == timing.llmMark {
		a.writingLLMTime = a.persistedLLMTime
	}
	if a.writingToolTime == timing.toolMark {
		a.writingToolTime = a.persistedToolTime
	}
}

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
	// persisted*Time is the clock already written to the durable per-model rows;
	// writing*Time additionally covers a write still in flight. Both die with
	// the runtime, so an evicted runtime's uncommitted slice is lost — the same
	// limitation as every other figure the runtime holds.
	persistedLLMTime  time.Duration
	persistedToolTime time.Duration
	writingLLMTime    time.Duration
	writingToolTime   time.Duration
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
		// DiscardUsage now protects Handover calls, so the ledger can carry the
		// true category. Recording it as subagent instead made the observed
		// breakdown disagree with the same spend in recorded history.
		a.stats.AddHandoverUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
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
func statsMS(d time.Duration) *int64 { return msPointer(d.Milliseconds()) }
func msPointer(ms int64) *int64      { return &ms }
func appendMainCallStats(out *serveStatsResponse, calls []ui.UsageCall) {
	var ttft, generation time.Duration
	observed, tokens := 0, 0
	for _, call := range calls {
		if call.ObservedOutput && call.GenerationTime > 0 {
			observed++
			ttft += call.TTFT
			generation += call.GenerationTime
			tokens += call.OutputTokens
		}
	}
	if observed > 0 {
		out.TTFTMS = statsMS(ttft / time.Duration(observed))
		value := float64(tokens) / generation.Seconds()
		out.OutputTokensPerSecond = &value
	}
	// Delegated requests are relayed into this ledger for the per-model table,
	// but each one also bills its own child session. Pricing them here and again
	// from the child rows would double every delegated turn, so the runtime's
	// figure covers the session's own spend only.
	own := make([]ui.UsageCall, 0, len(calls))
	for _, call := range calls {
		if !call.Subagent {
			own = append(own, call)
		}
	}
	estimate := ui.PriceUsageCalls(own, "")
	if estimate.Priced > 0 {
		cost := estimate.CostUSD
		out.CostUSD = &cost
	}
	out.CostPartial = estimate.Partial()
}

// usageCallKind names why a retained request was billed, using the same
// vocabulary as the persisted attribution so both scopes read alike.
func usageCallKind(call ui.UsageCall) session.ModelUsageKind {
	return session.ModelUsageKind(call.Kind())
}

// modelStatsAccumulator groups per-model rows and keeps each model's request
// boundaries so pricing tiers stay per request.
type modelStatsAccumulator struct {
	groups map[string]*serveStatsModel
	kinds  map[string]map[session.ModelUsageKind]bool
	costs  map[string][]ui.UsageCall
	order  []string
}

func newModelStatsAccumulator() *modelStatsAccumulator {
	return &modelStatsAccumulator{
		groups: map[string]*serveStatsModel{},
		kinds:  map[string]map[session.ModelUsageKind]bool{},
		costs:  map[string][]ui.UsageCall{},
	}
}

func (a *modelStatsAccumulator) group(model string) *serveStatsModel {
	model = strings.TrimSpace(model)
	if model == "" {
		model = session.UnknownUsageModel
	}
	if a.groups[model] == nil {
		a.groups[model] = &serveStatsModel{Model: model}
		a.kinds[model] = map[session.ModelUsageKind]bool{}
		a.order = append(a.order, model)
	}
	return a.groups[model]
}

// addUsage records one priced request against a model.
func (a *modelStatsAccumulator) addUsage(model string, kind session.ModelUsageKind, u llm.Usage, turns int) {
	row := a.group(model)
	row.InputTokens += u.InputTokens
	row.OutputTokens += u.OutputTokens
	row.CachedInputTokens += u.CachedInputTokens
	row.CacheWriteTokens += u.CacheWriteTokens
	row.LLMTurns += turns
	a.kinds[row.Model][kind] = true
	a.costs[row.Model] = append(a.costs[row.Model], ui.UsageCall{
		Model: row.Model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CachedInputTokens: u.CachedInputTokens, CacheWriteTokens: u.CacheWriteTokens,
	})
}

// modelPricing selects how much truth a cost estimate can carry.
type modelPricing int

const (
	// pricingNone leaves cost unset.
	pricingNone modelPricing = iota
	// pricingPerRequest prices retained request boundaries, so tiered rates apply
	// exactly as they were billed.
	pricingPerRequest
	// pricingAggregate prices pre-aggregated counters, one priced unit per
	// recorded row rather than per original request. Tiers cannot be
	// reconstructed from summed counters, so the number is an estimate — but an
	// estimate of what a session spent beats a dash.
	pricingAggregate
)

// flush appends the rows in stable model order.
func (a *modelStatsAccumulator) flush(out *serveStatsResponse, pricing modelPricing) {
	sort.Strings(a.order)
	for _, model := range a.order {
		row := a.groups[model]
		if pricing != pricingNone {
			var estimate ui.SessionStatsCostEstimate
			if pricing == pricingAggregate {
				// Recorded rows are sums, so they are priced at base rates and
				// reported as a floor rather than replayed as one huge request.
				estimate = ui.PriceAggregateUsage(row.Model, row.InputTokens, row.OutputTokens, row.CachedInputTokens, row.CacheWriteTokens)
			} else {
				estimate = ui.PriceUsageCalls(a.costs[model], "")
			}
			if estimate.Priced > 0 {
				cost := estimate.CostUSD
				row.CostUSD = &cost
			}
			row.CostPartial = estimate.Partial()
		}
		for _, kind := range []session.ModelUsageKind{
			session.ModelUsageMain, session.ModelUsageGuardian, session.ModelUsageCompaction,
			session.ModelUsageSideQuestion, session.ModelUsageHandover, session.ModelUsageSubagent,
		} {
			if a.kinds[model][kind] {
				row.Kinds = append(row.Kinds, string(kind))
			}
		}
		out.Models = append(out.Models, *row)
	}
}

// initRowTiming makes both clocks present. They are initialised together
// because the row may already carry one of them: a model that both ran the
// session's turns and appeared as a delegated run would otherwise reach an
// accumulation with a nil counterpart.
func initRowTiming(row *serveStatsModel) {
	if row.ActiveMS == nil {
		row.ActiveMS = statsMS(0)
	}
	if row.ToolMS == nil {
		row.ToolMS = statsMS(0)
	}
}

// appendRuntimeModelStats builds the observed per-model breakdown.
//
// Tokens come from the retained request ledger rather than the subagent
// tracker: the ledger is what the session totals are made of, and it is the
// only source that contains the session's own turns. The tracker supplies the
// tool counts and elapsed time of delegated runs.
//
// toolTime is the runtime's own tool clock, which is measured for the session
// as a whole rather than per request. It is attributed to the models that ran
// the session's own turns, because those are the turns that called the tools.
func appendRuntimeModelStats(out *serveStatsResponse, calls []ui.UsageCall, runs []ui.SubagentProgress, started map[string]bool, now time.Time, toolTime time.Duration) {
	accumulator := newModelStatsAccumulator()
	mainModels := map[string]bool{}
	for _, call := range calls {
		kind := usageCallKind(call)
		accumulator.addUsage(call.Model, kind, llm.Usage{
			InputTokens: call.InputTokens, OutputTokens: call.OutputTokens,
			CachedInputTokens: call.CachedInputTokens, CacheWriteTokens: call.CacheWriteTokens,
		}, 1)
		// The request ledger times each request it observed, so the session's
		// own model no longer has to report a dash next to its tokens.
		if call.GenerationTime > 0 {
			row := accumulator.group(call.Model)
			initRowTiming(row)
			*row.ActiveMS += call.GenerationTime.Milliseconds()
		}
		if kind == session.ModelUsageMain {
			mainModels[accumulator.group(call.Model).Model] = true
		}
	}
	// Tool time belongs to the session's own turns. With one model that is
	// exact; with several it is split evenly rather than silently parked on
	// whichever happened to run last.
	if toolTime > 0 && len(mainModels) > 0 {
		share := toolTime.Milliseconds() / int64(len(mainModels))
		for model := range mainModels {
			row := accumulator.group(model)
			initRowTiming(row)
			*row.ToolMS += share
			*row.ActiveMS += share
		}
	}
	for _, run := range runs {
		// Usage alone does not establish a run start time. Do not manufacture
		// timing for callbacks whose initialization was not observed.
		if !started[run.ToolCallID] && run.ToolCalls == 0 {
			continue
		}
		row := accumulator.group(run.ResolvedModel)
		row.Running = row.Running || !run.Done
		row.ToolCalls += run.ToolCalls
		if started[run.ToolCallID] {
			elapsed, toolTime := run.Timing(now)
			initRowTiming(row)
			*row.ActiveMS += elapsed.Milliseconds()
			*row.ToolMS += toolTime.Milliseconds()
		}
	}
	accumulator.flush(out, pricingPerRequest)
}

// appendDurableModelStats builds the recorded-history breakdown from persisted
// per-model attribution. Timing and request boundaries were never retained, so
// the rows report token counters priced as a single aggregate estimate.
//
// It also reports whatever the rows do not account for. Sessions that ran
// before attribution existed keep their whole bucket there, and any future call
// site that updates the session totals without attributing them shows up as a
// growing remainder rather than as a quietly wrong table.
func appendDurableModelStats(out *serveStatsResponse, entries []session.ModelUsage, totals webSessionMetrics) {
	accumulator := newModelStatsAccumulator()
	var attributed webSessionMetrics
	for _, entry := range entries {
		row := accumulator.group(entry.Model)
		row.ToolCalls += entry.ToolCalls
		if entry.LLMMS > 0 || entry.ToolMS > 0 {
			initRowTiming(row)
			*row.ActiveMS += entry.LLMMS + entry.ToolMS
			*row.ToolMS += entry.ToolMS
		}
		accumulator.addUsage(entry.Model, entry.Kind, llm.Usage{
			InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens,
			CachedInputTokens: entry.CachedInputTokens, CacheWriteTokens: entry.CacheWriteTokens,
		}, entry.LLMTurns)
		attributed.InputTokens += entry.InputTokens
		attributed.OutputTokens += entry.OutputTokens
		attributed.CachedInputTokens += entry.CachedInputTokens
		attributed.CacheWriteTokens += entry.CacheWriteTokens
	}
	if remainder, ok := unattributedTotals(totals, attributed); ok {
		out.Unattributed = &remainder
	}
	if len(entries) == 0 {
		return
	}
	accumulator.flush(out, pricingAggregate)
	out.Unavailable = removeUnavailable(out.Unavailable, "models")
	out.Unavailable = removeUnavailable(out.Unavailable, "historical_model_usage")
}

// aggregateCost prices summed counters as one estimate for a model. Recorded
// counters carry no request boundaries, so tiers cannot be reconstructed; an
// estimate of what was spent still beats no number at all.
func aggregateCost(model string, m webSessionMetrics) (*float64, bool) {
	estimate := ui.PriceAggregateUsage(strings.TrimSpace(model), m.InputTokens, m.OutputTokens, m.CachedInputTokens, m.CacheWriteTokens)
	if estimate.Priced == 0 {
		return nil, true
	}
	cost := estimate.CostUSD
	return &cost, estimate.Partial()
}

// sessionTimingFromModels totals the recorded model and tool time. Durable rows
// span every runtime the session ever had, which is what "how long has this
// session worked" means.
func sessionTimingFromModels(entries []session.ModelUsage) (llmMS, toolMS int64) {
	for _, entry := range entries {
		llmMS += max(0, entry.LLMMS)
		toolMS += max(0, entry.ToolMS)
	}
	return llmMS, toolMS
}

// durableCostSplit separates what a session spent on its own turns from what it
// spent on delegated ones. They are recorded differently depending on where the
// session ran — a TUI parent folds delegated usage into its own bucket and
// attributes it as subagent, a serve parent leaves it on the child sessions — so
// keeping them apart is what lets both worlds be totalled the same way.
type durableCostSplit struct {
	Own, Delegated float64
	Priced         bool
	Partial        bool
}

func durableCostFromEntries(entries []session.ModelUsage) durableCostSplit {
	var split durableCostSplit
	for _, entry := range entries {
		cost, partial := aggregateCost(entry.Model, webSessionMetrics{
			InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens,
			CachedInputTokens: entry.CachedInputTokens, CacheWriteTokens: entry.CacheWriteTokens,
		})
		split.Partial = split.Partial || partial
		if cost == nil {
			continue
		}
		split.Priced = true
		if entry.Kind == session.ModelUsageSubagent {
			split.Delegated += *cost
			continue
		}
		split.Own += *cost
	}
	return split
}

// unattributedTotals returns the positive part of totals minus attributed, and
// whether any of it remains. Negative components mean more was attributed than
// the bucket holds; that is drift too, so it is reported as zero rather than
// silently cancelling another counter.
func unattributedTotals(totals, attributed webSessionMetrics) (webSessionMetrics, bool) {
	remainder := webSessionMetrics{
		InputTokens:       max(0, totals.InputTokens-attributed.InputTokens),
		OutputTokens:      max(0, totals.OutputTokens-attributed.OutputTokens),
		CachedInputTokens: max(0, totals.CachedInputTokens-attributed.CachedInputTokens),
		CacheWriteTokens:  max(0, totals.CacheWriteTokens-attributed.CacheWriteTokens),
	}
	found := remainder.InputTokens > 0 || remainder.OutputTokens > 0 ||
		remainder.CachedInputTokens > 0 || remainder.CacheWriteTokens > 0
	return remainder, found
}

// removeUnavailable drops a capability that turned out to be available after
// the conservative default was declared. It copies rather than filtering in
// place so a retained caller slice is never rewritten underneath it.
func removeUnavailable(values []string, key string) []string {
	if values == nil {
		return nil
	}
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value != key {
			filtered = append(filtered, value)
		}
	}
	return filtered
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
		appendUsageSection(out, kind, count, usage)
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
	appendRuntimeModelStats(out, calls, runs, started, now, s.ToolTime)
	appendHelperUsageSections(out, calls)
	appendUsageSection(out, "Handover / Path-Note Usage", handoverCalls, handoverUsage)
	if retries > 0 || running || s.LLMCallCount > 0 || s.ToolCallCount > 0 {
		out.Sections = append(out.Sections, serveStatsSection{Title: "Runtime Activity", Rows: []serveStatsRow{statsRow("Retries", retries), statsRow("Running", running), statsRow("LLM calls", s.LLMCallCount), statsRow("Tool calls", s.ToolCallCount)}})
	}
	return out
}

// hasUnrecordedWork reports whether the runtime has billed work that the
// durable rows do not carry yet. A cost total built from those rows is a lower
// bound while that is true.
func (rt *serveRuntime) hasUnrecordedWork() bool {
	a := &rt.stats
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stats == nil {
		return false
	}
	return a.running || !a.attempt.BillableCountersZero()
}
