package cmd

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sidequestion"
)

type serveStatsRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
type serveStatsSection struct {
	Title string          `json:"title"`
	Rows  []serveStatsRow `json:"rows"`
}
type serveStatsModel struct {
	webSessionMetrics
	Model       string   `json:"model"`
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
	DurableMetrics        *webSessionMetrics  `json:"durable_metrics,omitempty"`
	ActiveMS              *int64              `json:"active_ms,omitempty"`
	ModelMS               *int64              `json:"model_ms,omitempty"`
	ToolMS                *int64              `json:"tool_ms,omitempty"`
	TTFTMS                *int64              `json:"ttft_ms,omitempty"`
	OutputTokensPerSecond *float64            `json:"output_tokens_per_second,omitempty"`
	CostUSD               *float64            `json:"cost_usd,omitempty"`
	CostPartial           bool                `json:"cost_partial,omitempty"`
	Models                []serveStatsModel   `json:"models"`
	Sections              []serveStatsSection `json:"sections"`
	Scope                 string              `json:"scope"`
	Unavailable           []string            `json:"unavailable"`
}

func statsRow(label string, value any) serveStatsRow { return serveStatsRow{label, fmt.Sprint(value)} }
func statsUsageSection(title string, n int, u llm.Usage) serveStatsSection {
	input := u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens
	rows := []serveStatsRow{statsRow("Requests", n), statsRow("Fresh input tokens", u.InputTokens), statsRow("Cache read tokens", u.CachedInputTokens), statsRow("Cache write tokens", u.CacheWriteTokens), statsRow("Output tokens", u.OutputTokens), statsRow("Total tokens", input+u.OutputTokens)}
	if input > 0 {
		rows = append(rows, statsRow("Cache hit rate", fmt.Sprintf("%.1f%%", 100*float64(u.CachedInputTokens)/float64(input))))
	}
	return serveStatsSection{title, rows}
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
func (s *serveServer) handleSessionStats(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, 405, "method_not_allowed", "method not allowed")
		return
	}
	var meta *session.Session
	var messages []session.Message
	if s.store != nil {
		var err error
		meta, err = s.store.Get(r.Context(), id)
		if err != nil && !errors.Is(err, session.ErrNotFound) {
			writeOpenAIError(w, 500, "server_error", "failed to read session")
			return
		}
		if meta == nil {
			http.NotFound(w, r)
			return
		}
		messages, err = s.store.GetMessages(r.Context(), id, 0, 0)
		if err != nil {
			writeOpenAIError(w, 500, "server_error", "failed to read session history")
			return
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
		for i, m := range sidequestion.CloneMessages(rt.history) {
			messages = append(messages, *session.NewMessage(id, m, i))
		}
		rt.mu.Unlock()
	}
	if meta == nil && rt == nil {
		http.NotFound(w, r)
		return
	}
	var out *serveStatsResponse
	if rt != nil {
		out = rt.statsSnapshot()
	}
	if out == nil {
		out = &serveStatsResponse{Scope: "durable_history", Models: []serveStatsModel{}, Sections: []serveStatsSection{}, Unavailable: []string{"runtime_metrics", "active_ms", "model_ms", "tool_ms", "ttft_ms", "output_tokens_per_second", "cost_usd", "models", "helper_usage"}}
	}
	if out.Scope == "durable_history" {
		for _, title := range []string{"Private Side-Question Usage", "Guardian Usage", "Compaction Usage", "Handover / Path-Note Usage", "Runtime Activity"} {
			out.Sections = append(out.Sections, serveStatsSection{title, []serveStatsRow{statsRow("Availability", "unavailable — no runtime observation retained")}})
		}
	}
	out.Unavailable = append(out.Unavailable, "historical_timing", "historical_model_usage", "historical_cost", "historical_helper_usage")
	if out.Scope == "runtime_local" {
		if out.TTFTMS == nil {
			out.Unavailable = append(out.Unavailable, "ttft_ms", "output_tokens_per_second")
		}
		if out.CostUSD == nil {
			out.Unavailable = append(out.Unavailable, "cost_usd")
		}
	}
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
			for _, m := range messages {
				if m.Sequence >= meta.CompactionSeq {
					active = append(active, m)
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
		if d, ok := rt.engine.ToolDiscoveryDiagnostics(id); ok {
			rows := []serveStatsRow{statsRow("Mode configured/resolved", d.ConfiguredMode+" / "+d.ResolvedMode), statsRow("Strategy configured/resolved", d.ConfiguredStrategy+" / "+d.Strategy), statsRow("Mode reason", d.Reason), statsRow("Strategy reason", d.StrategyReason), statsRow("Native fallback", fmt.Sprintf("%d (%s)", d.FallbackCount, d.FallbackReason)), statsRow("Pinned MCP", fmt.Sprintf("%d tools, ~%d tokens", d.PinnedCount, d.PinnedTokens)), statsRow("Active MCP", fmt.Sprintf("%d tools, ~%d tokens", d.ActiveMCPCount, d.ActiveMCPTokens)), statsRow("Deferred MCP", fmt.Sprintf("%d tools, ~%d tokens avoided", d.DeferredCount, d.DeferredTokens)), statsRow("Dynamic working set", fmt.Sprintf("%d/%d tools", d.DynamicActive, d.DynamicLimit)), statsRow("Working-set evictions", d.EvictionCount)}
			for _, v := range d.RecentEvictions {
				rows = append(rows, statsRow("Recent eviction", v.Name+" — "+v.Reason))
			}
			for _, v := range d.Recent {
				rows = append(rows, statsRow("Recent activation", v.Name+" — "+v.Reason))
			}
			out.Sections = append(out.Sections, serveStatsSection{"Tool Discovery", rows})
		} else {
			out.Unavailable = append(out.Unavailable, "tool_discovery")
			out.Sections = append(out.Sections, serveStatsSection{"Tool Discovery", []serveStatsRow{statsRow("Availability", "unavailable — no discovery diagnostics")}})
		}
	} else {
		out.Unavailable = append(out.Unavailable, "tool_discovery", "compaction_thresholds")
		out.Sections = append(out.Sections, serveStatsSection{"Tool Discovery", []serveStatsRow{statsRow("Availability", "unavailable — no runtime retained")}})
	}
	estimate := func(ms []session.Message) int {
		llmMessages := make([]llm.Message, 0, len(ms))
		for _, m := range ms {
			llmMessages = append(llmMessages, m.ToLLMMessage())
		}
		return llm.EstimateMessageTokens(llmMessages)
	}
	source := "last reported context"
	if used <= 0 {
		used = estimate(active)
		source = "message estimate (excludes request-only tool schemas and prompt overhead)"
	}
	history := max(used, estimate(messages))
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
	u := out.Metrics
	out.Sections = append(out.Sections, statsUsageSection("Cumulative Token Usage · "+out.Scope, u.LLMTurns, llm.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedInputTokens: u.CachedInputTokens, CacheWriteTokens: u.CacheWriteTokens}))
	roles := map[llm.Role]int{}
	for _, m := range active {
		roles[m.Role]++
	}
	activity := []serveStatsRow{statsRow("Active messages", len(active)), statsRow("User messages", roles[llm.RoleUser]), statsRow("Assistant messages", roles[llm.RoleAssistant]), statsRow("Tool messages", roles[llm.RoleTool])}
	if meta != nil {
		activity = append(activity, statsRow("User turns (durable)", meta.UserTurns), statsRow("Assistant turns (durable)", meta.LLMTurns), statsRow("Status", meta.Status))
		boundary := "none"
		if session.HasCompactionBoundary(meta) {
			boundary = fmt.Sprintf("seq %d (%d messages outside context)", meta.CompactionSeq, len(messages)-len(active))
		}
		out.Sections = append(out.Sections, serveStatsSection{"Compactions", []serveStatsRow{statsRow("Compactions", meta.CompactionCount), statsRow("Last boundary", boundary)}})
	}
	if rt != nil {
		rt.sideQuestion.mu.Lock()
		successful := len(rt.sideQuestion.history)
		running := rt.sideQuestion.running
		rt.sideQuestion.mu.Unlock()
		out.Sections = append(out.Sections, serveStatsSection{"Private Side-Question History", []serveStatsRow{statsRow("Successful history", successful), statsRow("Running", running)}})
		activity = append(activity, statsRow("Compacting", rt.compacting.Load()), statsRow("Admitted activity", rt.admittedActivity.Load()))
	}
	out.Sections = append(out.Sections, serveStatsSection{"Cumulative Session Activity", activity}, serveStatsSection{"Availability", []serveStatsRow{statsRow("Scope", out.Scope), statsRow("Runtime accounting", "Since this runtime was created; resets on eviction, replacement or process restart. Historical detail is unavailable."), statsRow("Models", "Subagent billing models only; helper usage is attributed to its actual model. Times sum across child runs."), statsRow("Timing", "Main request and tool time only; failed-attempt time retained, idle and retry backoff excluded. Parallel tool time is wall-clock union, not summed calls. TTFT and throughput use retained observed-output calls."), statsRow("Cost", "Local per-request estimates; missing prices are unavailable, partial costs are lower bounds.")}})
	writeJSON(w, 200, out)
}
