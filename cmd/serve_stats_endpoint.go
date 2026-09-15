package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sidequestion"
)

type serveStatsRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
	// Exact carries the unabbreviated value for a hover title, and is set only
	// when the displayed value actually hides digits.
	Exact string `json:"exact,omitempty"`
}
type serveStatsSection struct {
	Title string          `json:"title"`
	Rows  []serveStatsRow `json:"rows"`
}
type serveStatsModel struct {
	webSessionMetrics
	Model string `json:"model"`
	// Kinds names why the model was billed (main, guardian, compaction,
	// side_question, handover, subagent). A session's own model is billed as
	// "main"; without it the breakdown silently omits the largest spender.
	Kinds       []string `json:"kinds,omitempty"`
	Running     bool     `json:"running,omitempty"`
	ActiveMS    *int64   `json:"active_ms,omitempty"`
	ToolMS      *int64   `json:"tool_ms,omitempty"`
	CostUSD     *float64 `json:"cost_usd,omitempty"`
	CostPartial bool     `json:"cost_partial,omitempty"`
}
type serveStatsResponse struct {
	Metrics webSessionMetrics `json:"metrics"`
	// DurableMetrics is deliberately separate: metrics/timing/cost share the
	// runtime-local scope whenever observation exists, never a fabricated blend.
	DurableMetrics        *webSessionMetrics `json:"durable_metrics,omitempty"`
	ActiveMS              *int64             `json:"active_ms,omitempty"`
	ModelMS               *int64             `json:"model_ms,omitempty"`
	ToolMS                *int64             `json:"tool_ms,omitempty"`
	TTFTMS                *int64             `json:"ttft_ms,omitempty"`
	OutputTokensPerSecond *float64           `json:"output_tokens_per_second,omitempty"`
	CostUSD               *float64           `json:"cost_usd,omitempty"`
	CostPartial           bool               `json:"cost_partial,omitempty"`
	// Unattributed is the part of the recorded totals that no per-model row
	// claims: spend from before attribution was recorded, or drift if a future
	// call site ever updates the session bucket without attributing it.
	Unattributed *webSessionMetrics  `json:"unattributed,omitempty"`
	Models       []serveStatsModel   `json:"models"`
	Sections     []serveStatsSection `json:"sections"`
	Scope        string              `json:"scope"`
	Unavailable  []string            `json:"unavailable"`
}

// statsRow renders one row. Counts are abbreviated so a column of millions can
// be compared at a glance, and carry their exact value for the client to reveal
// on hover; everything else is printed as given.
func statsRow(label string, value any) serveStatsRow {
	var n int64
	switch v := value.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	default:
		return serveStatsRow{Label: label, Value: fmt.Sprint(value)}
	}
	row := serveStatsRow{Label: label, Value: compactCount(n)}
	if exact := groupedCount(n); exact != row.Value {
		row.Exact = exact
	}
	return row
}

// compactCount abbreviates with one decimal: 12.1K, 17.3M, 1.2B. The unit is
// chosen after rounding, so 999,950 reads 1M rather than 1000K.
func compactCount(n int64) string {
	abs := float64(n)
	if abs < 0 {
		abs = -abs
	}
	for _, unit := range []struct {
		scale  float64
		suffix string
	}{{1_000_000_000, "B"}, {1_000_000, "M"}, {1_000, "K"}} {
		// Promote only once the smaller unit would round to a full thousand of
		// itself, so 999,950 reads 1M while 955,470 stays 955.5K.
		if abs >= unit.scale*0.9995 {
			return trimCompact(float64(n)/unit.scale) + unit.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// trimCompact rounds half away from zero, matching the client's formatter, and
// drops a trailing ".0".
func trimCompact(value float64) string {
	rounded := math.Round(value*10) / 10
	return strings.TrimSuffix(strconv.FormatFloat(rounded, 'f', 1, 64), ".0")
}

// groupedCount is the exact value with thousands separators.
func groupedCount(n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var out strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return sign + out.String()
}

// appendUsageSection adds a helper-usage section only when the helper actually
// ran. A wall of all-zero sections buries the counters that do carry data.
func appendUsageSection(out *serveStatsResponse, title string, n int, u llm.Usage) {
	if n == 0 && u.BillableCountersZero() {
		return
	}
	input := u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens
	rows := []serveStatsRow{statsRow("Requests", n), statsRow("Fresh input tokens", u.InputTokens), statsRow("Cache read tokens", u.CachedInputTokens), statsRow("Cache write tokens", u.CacheWriteTokens), statsRow("Output tokens", u.OutputTokens), statsRow("Total tokens", input+u.OutputTokens)}
	if input > 0 {
		rows = append(rows, statsRow("Cache hit rate", fmt.Sprintf("%.1f%%", 100*float64(u.CachedInputTokens)/float64(input))))
	}
	out.Sections = append(out.Sections, serveStatsSection{title, rows})
}

// peekStatsRuntime does not renew the runtime's idle TTL (unlike Get).
func (s *serveServer) peekStatsRuntime(id string) *serveRuntime {
	if s.sessionMgr == nil {
		return nil
	}
	s.sessionMgr.mu.Lock()
	defer s.sessionMgr.mu.Unlock()
	return s.sessionMgr.sessions[id]
}
func (s *serveServer) loadStatsInputs(w http.ResponseWriter, r *http.Request, id string) (*session.Session, []session.Message, *serveRuntime, bool) {
	var meta *session.Session
	var messages []session.Message
	if s.store != nil {
		var err error
		meta, err = s.store.Get(r.Context(), id)
		if err != nil && !errors.Is(err, session.ErrNotFound) {
			writeOpenAIError(w, 500, "server_error", "failed to read session")
			return nil, nil, nil, false
		}
		if meta == nil {
			http.NotFound(w, r)
			return nil, nil, nil, false
		}
		messages, err = s.store.GetMessages(r.Context(), id, 0, 0)
		if err != nil {
			writeOpenAIError(w, 500, "server_error", "failed to read session history")
			return nil, nil, nil, false
		}
	}
	rt := s.peekStatsRuntime(id)
	if rt != nil && meta == nil && rt.mu.TryLock() {
		if rt.sessionMeta != nil {
			copy := *rt.sessionMeta
			meta = &copy
		}
		// Parts (including tool arguments/results) remain mutable after the
		// run lock is released; detach them before estimating the snapshot.
		for i, message := range sidequestion.CloneMessages(rt.history) {
			messages = append(messages, *session.NewMessage(id, message, i))
		}
		rt.mu.Unlock()
	}
	if meta == nil && rt == nil {
		http.NotFound(w, r)
		return nil, nil, nil, false
	}
	return meta, messages, rt, true
}

func statsResponseForRuntime(rt *serveRuntime) *serveStatsResponse {
	var out *serveStatsResponse
	if rt != nil {
		out = rt.statsSnapshot()
	}
	if out == nil {
		out = &serveStatsResponse{Scope: "durable_history", Models: []serveStatsModel{}, Sections: []serveStatsSection{}, Unavailable: []string{"runtime_metrics", "active_ms", "model_ms", "tool_ms", "ttft_ms", "output_tokens_per_second", "cost_usd", "models", "helper_usage"}}
	}
	// Recorded history retains none of the helper or runtime detail. Listing
	// each missing section as its own "unavailable" block said the same thing
	// five times and buried the counters that do exist; Unavailable already
	// carries the same information for callers that need it.
	out.Unavailable = append(out.Unavailable, "historical_timing", "historical_model_usage", "historical_cost", "historical_helper_usage")
	if out.Scope == "runtime_local" {
		if out.TTFTMS == nil {
			out.Unavailable = append(out.Unavailable, "ttft_ms", "output_tokens_per_second")
		}
		if out.CostUSD == nil {
			out.Unavailable = append(out.Unavailable, "cost_usd")
		}
	}
	return out
}

func estimateStatsMessageTokens(messages []session.Message) int {
	llmMessages := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		llmMessages = append(llmMessages, message.ToLLMMessage())
	}
	return llm.EstimateMessageTokens(llmMessages)
}

func appendStatsContextSections(out *serveStatsResponse, meta *session.Session, messages []session.Message, rt *serveRuntime, id string) []session.Message {
	used, limit, soft, hard := 0, 0, 0, 0
	compactEnabled := false
	provider, model := "unknown", "unknown"
	active := messages
	if meta != nil {
		durable := webSessionMetrics{InputTokens: meta.InputTokens, OutputTokens: meta.OutputTokens, CachedInputTokens: meta.CachedInputTokens, CacheWriteTokens: meta.CacheWriteTokens, ToolCalls: meta.ToolCalls, LLMTurns: meta.LLMTurns}
		if out.Scope == "durable_history" {
			out.Metrics = durable
		} else {
			out.DurableMetrics = &durable
		}
		provider, model = meta.ProviderKey, meta.Model
		used = meta.LastTotalTokens
		limit = llm.InputLimitForProviderModel(provider, model)
		if session.HasCompactionBoundary(meta) {
			active = nil
			for _, message := range messages {
				if message.Sequence >= meta.CompactionSeq {
					active = append(active, message)
				}
			}
		}
	}
	if rt != nil && rt.engine != nil {
		if n := rt.engine.LastTotalTokens(); n > 0 {
			used = n
		}
		if n := rt.engine.InputLimit(); n > 0 {
			limit = n
		}
		soft, hard, compactEnabled = rt.engine.CompactionThresholds()
		if diagnostics, ok := rt.engine.ToolDiscoveryDiagnostics(id); ok {
			rows := []serveStatsRow{statsRow("Mode configured/resolved", diagnostics.ConfiguredMode+" / "+diagnostics.ResolvedMode), statsRow("Strategy configured/resolved", diagnostics.ConfiguredStrategy+" / "+diagnostics.Strategy), statsRow("Mode reason", diagnostics.Reason), statsRow("Strategy reason", diagnostics.StrategyReason), statsRow("Native fallback", fmt.Sprintf("%d (%s)", diagnostics.FallbackCount, diagnostics.FallbackReason)), statsRow("Pinned MCP", fmt.Sprintf("%d tools, ~%d tokens", diagnostics.PinnedCount, diagnostics.PinnedTokens)), statsRow("Active MCP", fmt.Sprintf("%d tools, ~%d tokens", diagnostics.ActiveMCPCount, diagnostics.ActiveMCPTokens)), statsRow("Deferred MCP", fmt.Sprintf("%d tools, ~%d tokens avoided", diagnostics.DeferredCount, diagnostics.DeferredTokens)), statsRow("Dynamic working set", fmt.Sprintf("%d/%d tools", diagnostics.DynamicActive, diagnostics.DynamicLimit)), statsRow("Working-set evictions", diagnostics.EvictionCount)}
			for _, value := range diagnostics.RecentEvictions {
				rows = append(rows, statsRow("Recent eviction", value.Name+" — "+value.Reason))
			}
			for _, value := range diagnostics.Recent {
				rows = append(rows, statsRow("Recent activation", value.Name+" — "+value.Reason))
			}
			out.Sections = append(out.Sections, serveStatsSection{"Tool Discovery", rows})
		} else {
			out.Unavailable = append(out.Unavailable, "tool_discovery")
		}
	} else {
		out.Unavailable = append(out.Unavailable, "tool_discovery", "compaction_thresholds")
	}
	source := "last reported context"
	if used <= 0 {
		used = estimateStatsMessageTokens(active)
		source = "message estimate"
	}
	history := max(used, estimateStatsMessageTokens(messages))
	pressure := []serveStatsRow{statsRow("Provider / model", provider+" / "+model), statsRow("Current context", used), statsRow("Context source", source), statsRow("Cumulative history (estimated)", history), statsRow("Outside context (estimated)", max(0, history-used))}
	if limit > 0 {
		pressure = append(pressure, statsRow("Input limit", limit), statsRow("Window used", fmt.Sprintf("%.1f%%", 100*float64(used)/float64(limit))), statsRow("Free space", max(0, limit-used)))
	} else {
		pressure = append(pressure, statsRow("Input limit", "unavailable"))
	}
	if rt != nil && rt.engine != nil {
		pressure = append(pressure, statsRow("Compaction enabled", compactEnabled))
	} else {
		pressure = append(pressure, statsRow("Compaction enabled", "unavailable"))
	}
	if compactEnabled {
		pressure = append(pressure,
			statsRow("Soft compact at", soft), statsRow("Soft window buffer", max(0, limit-soft)),
			statsRow("Hard compact at", hard), statsRow("Hard window buffer", max(0, limit-hard)))
	}
	out.Sections = append(out.Sections, serveStatsSection{"Current Context / Window Pressure", pressure})
	usage := out.Metrics
	appendUsageSection(out, "Cumulative Token Usage · "+out.Scope, usage.LLMTurns, llm.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CachedInputTokens: usage.CachedInputTokens, CacheWriteTokens: usage.CacheWriteTokens})
	return active
}

func appendStatsActivitySections(out *serveStatsResponse, meta *session.Session, messages, active []session.Message, rt *serveRuntime) {
	roles := map[llm.Role]int{}
	for _, message := range active {
		roles[message.Role]++
	}
	activity := []serveStatsRow{statsRow("Active messages", len(active)), statsRow("User messages", roles[llm.RoleUser]), statsRow("Assistant messages", roles[llm.RoleAssistant]), statsRow("Tool messages", roles[llm.RoleTool])}
	if meta != nil {
		// Session status is left out: "interrupted" describes the last run, not
		// the session, and a usage report is not where anyone learns it.
		activity = append(activity, statsRow("User turns (durable)", meta.UserTurns), statsRow("Assistant turns (durable)", meta.LLMTurns))
		// A session that never compacted has nothing to say here.
		if session.HasCompactionBoundary(meta) || meta.CompactionCount > 0 {
			boundary := "none"
			if session.HasCompactionBoundary(meta) {
				boundary = fmt.Sprintf("seq %d (%d messages outside context)", meta.CompactionSeq, len(messages)-len(active))
			}
			out.Sections = append(out.Sections, serveStatsSection{"Compactions", []serveStatsRow{statsRow("Compactions", meta.CompactionCount), statsRow("Last boundary", boundary)}})
		}
	}
	if rt != nil {
		rt.sideQuestion.mu.Lock()
		successful := len(rt.sideQuestion.history)
		running := rt.sideQuestion.running
		rt.sideQuestion.mu.Unlock()
		if successful > 0 || running {
			out.Sections = append(out.Sections, serveStatsSection{"Private Side-Question History", []serveStatsRow{statsRow("Successful history", successful), statsRow("Running", running)}})
		}
		// Compaction is worth a row while it is happening; "false" is not, and
		// the admitted-activity counter is scheduler bookkeeping.
		if rt.compacting.Load() {
			activity = append(activity, statsRow("Compacting", true))
		}
	}
	// Deliberately no Availability section. It restated the scope the UI
	// already labels and explained caveats for figures that are often not even
	// on screen. Where a caveat matters it belongs next to the number it
	// qualifies, not in a block of prose at the end.
	out.Sections = append(out.Sections, serveStatsSection{"Cumulative Session Activity", activity})
}

// applyDurableSessionTotals answers "what has this whole session spent, and how
// long has it worked" from the persisted per-model rows.
//
// Recorded history keeps only one undifferentiated token bucket per session, so
// those rows are the sole record of which model — including the session's own —
// spent what. They also span every runtime the session ever had, while a
// runtime only ever holds the slice billed since it was created, which is why
// they own the headline in both scopes.
// sessionDelegatedCost is what a session's child runs spent. They bill their own
// session rows, so the parent has to add them to report the real total.
type sessionDelegatedCost struct {
	CostUSD float64
	Partial bool
}

// delegatedCost prices every child session of a parent.
func (s *serveServer) delegatedCost(ctx context.Context, parentID string) sessionDelegatedCost {
	if s.store == nil || strings.TrimSpace(parentID) == "" {
		return sessionDelegatedCost{}
	}
	// Archiving a child does not unspend its tokens.
	children, err := s.store.List(ctx, session.ListOptions{ParentID: parentID, Limit: maxChildRunProjection, SortByActivity: true, Archived: true})
	if err != nil {
		log.Printf("[serve] child session cost unavailable for %s: %v", parentID, err)
		return sessionDelegatedCost{Partial: true}
	}
	var total sessionDelegatedCost
	for _, child := range children {
		cost, partial := aggregateCost(child.Model, webSessionMetrics{
			InputTokens: child.InputTokens, OutputTokens: child.OutputTokens,
			CachedInputTokens: child.CachedInputTokens, CacheWriteTokens: child.CacheWriteTokens,
		})
		if cost != nil {
			total.CostUSD += *cost
		}
		total.Partial = total.Partial || partial
	}
	// The list is capped, so a very wide fan-out reports a lower bound.
	total.Partial = total.Partial || len(children) >= maxChildRunProjection
	return total
}

func (s *serveServer) applyDurableSessionTotals(out *serveStatsResponse, rt *serveRuntime, entries []session.ModelUsage, delegated sessionDelegatedCost, totals webSessionMetrics) {
	// Recorded history shows the breakdown itself; a live runtime keeps its own
	// observed rows and only borrows the session-wide totals.
	breakdown := out
	if out.Scope != "durable_history" {
		breakdown = &serveStatsResponse{}
	}
	appendDurableModelStats(breakdown, entries, totals)
	// Spend no row claims belongs on screen in either scope: it is the only
	// evidence that the session's own totals are larger than the breakdown.
	if breakdown != out && breakdown.Unattributed != nil {
		out.Unattributed = breakdown.Unattributed
	}

	split := durableCostFromEntries(entries)
	// The recorded rows are the whole session; the runtime holds only the slice
	// it ran, so the two are not competing estimates of the same interval and
	// must not be maxed — after an eviction that would either hide the turn in
	// flight or erase everything that came before it. Recorded rows win, and the
	// runtime answers only for a session that has none yet.
	own, ownPriced := split.Own, split.Priced
	if !ownPriced && out.CostUSD != nil {
		own, ownPriced = *out.CostUSD, true
	}
	// A turn that has not been written yet is real spend this cannot see.
	inFlight := rt != nil && rt.hasUnrecordedWork()
	// Delegated work is recorded either on the parent (TUI) or on the child
	// sessions (serve). Both describe the same spend, so take the larger rather
	// than adding them and doubling every delegated turn.
	delegatedCost := max(split.Delegated, delegated.CostUSD)
	cost := own + delegatedCost
	priced := ownPriced || delegatedCost > 0
	// Any total is a lower bound when spend exists that the rows cannot account
	// for, a row could not be priced, or a turn has not been written yet.
	partial := split.Partial || breakdown.Unattributed != nil || delegated.Partial || inFlight
	if priced {
		out.CostUSD, out.CostPartial = &cost, partial || out.CostPartial
		for _, key := range []string{"cost_usd", "historical_cost"} {
			out.Unavailable = removeUnavailable(out.Unavailable, key)
		}
	}

	llmMS, toolMS := sessionTimingFromModels(entries)
	// The clock of a turn still running is not in the rows yet. Adding the
	// runtime's uncommitted remainder keeps a live session's tiles moving
	// instead of freezing at the last persisted turn.
	if rt != nil {
		pending := rt.pendingTurnTiming()
		llmMS, toolMS = llmMS+pending.LLMMS, toolMS+pending.ToolMS
	}
	if llmMS > 0 || toolMS > 0 {
		out.ModelMS, out.ToolMS, out.ActiveMS = msPointer(llmMS), msPointer(toolMS), msPointer(llmMS+toolMS)
		for _, key := range []string{"active_ms", "model_ms", "tool_ms", "historical_timing"} {
			out.Unavailable = removeUnavailable(out.Unavailable, key)
		}
	}
	if len(breakdown.Models) > 0 {
		out.Unavailable = removeUnavailable(out.Unavailable, "historical_model_usage")
	}
}

func (s *serveServer) handleSessionStats(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, 405, "method_not_allowed", "method not allowed")
		return
	}
	meta, messages, rt, ok := s.loadStatsInputs(w, r, id)
	if !ok {
		return
	}
	out := statsResponseForRuntime(rt)
	if s.store != nil && meta != nil {
		entries, err := s.store.ListModelUsage(r.Context(), id)
		if err != nil {
			// A failed read is not an empty attribution: reporting it as one
			// would drop the session's cost and turn every recorded token into
			// a phantom "not attributed" block.
			log.Printf("[serve] ListModelUsage failed for %s: %v", id, err)
			out.CostPartial = true
			out.Unavailable = append(out.Unavailable, "recorded_attribution")
		} else {
			// Run this even with no rows at all: a session recorded before
			// attribution existed has its whole bucket to report as
			// unattributed, and that is precisely the session that otherwise
			// shows nothing.
			s.applyDurableSessionTotals(out, rt, entries, s.delegatedCost(r.Context(), id), webSessionMetrics{
				InputTokens: meta.InputTokens, OutputTokens: meta.OutputTokens,
				CachedInputTokens: meta.CachedInputTokens, CacheWriteTokens: meta.CacheWriteTokens,
			})
		}
	}
	active := appendStatsContextSections(out, meta, messages, rt, id)
	appendStatsActivitySections(out, meta, messages, active, rt)
	writeJSON(w, 200, out)
}
