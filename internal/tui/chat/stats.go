package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminaltext"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

var statsCostEstimator = estimateStatsCost

func (m *Model) exitStatsSummary() string {
	if m == nil || !m.showStats || m.stats == nil || m.stats.LLMCallCount <= 0 {
		return ""
	}
	m.stats.Finalize()
	m.stats.ClearEstimatedCost()
	if cost, err := statsCostEstimator(m.statsPricingModel(), m.stats); err == nil {
		m.stats.SetEstimatedCost(cost)
	}
	return m.stats.Render()
}

func (m *Model) liveStatsSummary() string {
	if m == nil || m.stats == nil {
		return ""
	}
	// Price and render a value copy: opening /stats must not finalize the live
	// timer or attach transient pricing state to the session accumulator.
	snapshot := m.stats.SnapshotAt(time.Now())
	snapshot.ClearEstimatedCost()
	if cost, err := statsCostEstimator(m.statsPricingModel(), &snapshot); err == nil {
		snapshot.SetEstimatedCost(cost)
	}
	return strings.TrimPrefix(snapshot.Render(), "Stats: ")
}

func (m *Model) cmdStats() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	m.dialog.ShowContent("Chat Stats", m.renderStatsModal())
	return m, nil
}

func (m *Model) renderStatsModal() string {
	limit := 0
	if m.engine != nil {
		limit = m.engine.InputLimit()
	}
	if limit <= 0 {
		limit = llm.InputLimitForProviderModel(m.providerKey, m.modelName)
	}
	used := 0
	if m.engine != nil {
		used = m.engine.LastTotalTokens()
	}
	if used <= 0 {
		used = m.estimateContextTokensCached()
	}
	if used < 0 {
		used = 0
	}

	roleCounts := map[string]int{}
	allMessages, activeMessages := m.messageSnapshotsForStats()
	for _, msg := range activeMessages {
		roleCounts[string(msg.Role)]++
	}

	currentTokens := used
	if currentTokens <= 0 {
		currentTokens = m.estimateMessagesTokens(activeMessages)
	}
	historyTokens := m.estimateMessagesTokens(allMessages)
	if historyTokens < currentTokens {
		// Provider-reported current context can include prompt/tool overhead that
		// message-only estimation does not. Keep the comparison monotonic rather
		// than showing impossible history < current values.
		historyTokens = currentTokens
	}
	free := 0
	if limit > 0 {
		free = max(0, limit-currentTokens)
	}
	softThreshold, hardThreshold, compactionEnabled := 0, 0, false
	if m.engine != nil {
		softThreshold, hardThreshold, compactionEnabled = m.engine.CompactionThresholds()
	}

	var b strings.Builder
	if summary := m.liveStatsSummary(); summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}
	b.WriteString("Current Context / Window Pressure\n")
	b.WriteString(renderContextGrid(currentTokens, limit, softThreshold, hardThreshold))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("%s:%s\n", nonEmpty(m.providerKey, m.providerName), nonEmpty(m.modelName, "unknown-model")))
	if limit > 0 {
		b.WriteString(fmt.Sprintf("%s/%s tokens (%.1f%% used)\n", ui.FormatTokenCount(currentTokens), ui.FormatTokenCount(limit), percent(currentTokens, limit)))
	} else {
		b.WriteString(fmt.Sprintf("%s tokens used (context window unknown)\n", ui.FormatTokenCount(currentTokens)))
	}
	b.WriteString("\nCurrent context vs cumulative history\n")
	b.WriteString(fmt.Sprintf("■ Current context:   %-8s %5.1f%% of window\n", ui.FormatTokenCount(currentTokens), percent(currentTokens, limit)))
	b.WriteString(fmt.Sprintf("◆ Cumulative history: %-8s %5.1f%% of window\n", ui.FormatTokenCount(historyTokens), percent(historyTokens, limit)))
	if hidden := max(0, historyTokens-currentTokens); hidden > 0 {
		b.WriteString(fmt.Sprintf("· Outside context:   %-8s %5.1f%% of history\n", ui.FormatTokenCount(hidden), percent(hidden, historyTokens)))
	}
	if limit > 0 {
		b.WriteString(fmt.Sprintf("□ Free space:        %-8s %5.1f%% of window\n", ui.FormatTokenCount(free), percent(free, limit)))
	}
	if compactionEnabled {
		b.WriteString(fmt.Sprintf("× Soft compact at:  %-8s %5.1f%% (%s window buffer)\n", ui.FormatTokenCount(softThreshold), percent(softThreshold, limit), ui.FormatTokenCount(max(0, limit-softThreshold))))
		if hardThreshold != softThreshold {
			b.WriteString(fmt.Sprintf("! Hard compact at:  %-8s %5.1f%% (%s window buffer)\n", ui.FormatTokenCount(hardThreshold), percent(hardThreshold, limit), ui.FormatTokenCount(max(0, limit-hardThreshold))))
		}
	}

	if m.engine != nil {
		sessionID := ""
		if m.sess != nil {
			sessionID = m.sess.ID
		}
		if discovery, ok := m.engine.ToolDiscoveryDiagnostics(sessionID); ok {
			b.WriteString("\nTool Discovery\n")
			b.WriteString(fmt.Sprintf("Mode configured/resolved: %s / %s\n", discovery.ConfiguredMode, discovery.ResolvedMode))
			b.WriteString(fmt.Sprintf("Strategy configured/resolved: %s / %s\n", discovery.ConfiguredStrategy, discovery.Strategy))
			b.WriteString(fmt.Sprintf("Mode reason:          %s\n", discovery.Reason))
			b.WriteString(fmt.Sprintf("Strategy reason:      %s\n", discovery.StrategyReason))
			if discovery.FallbackCount > 0 {
				b.WriteString(fmt.Sprintf("Native fallback:      %d (%s)\n", discovery.FallbackCount, discovery.FallbackReason))
			}
			b.WriteString(fmt.Sprintf("Pinned MCP:          %d tools, ~%s tokens\n", discovery.PinnedCount, ui.FormatTokenCount(discovery.PinnedTokens)))
			b.WriteString(fmt.Sprintf("Active MCP:          %d tools, ~%s tokens\n", discovery.ActiveMCPCount, ui.FormatTokenCount(discovery.ActiveMCPTokens)))
			b.WriteString(fmt.Sprintf("Deferred MCP:        %d tools, ~%s tokens avoided\n", discovery.DeferredCount, ui.FormatTokenCount(discovery.DeferredTokens)))
			b.WriteString(fmt.Sprintf("Dynamic working set: %d/%d tools\n", discovery.DynamicActive, discovery.DynamicLimit))
			if discovery.EvictionCount > 0 {
				b.WriteString(fmt.Sprintf("Working-set evictions: %d\n", discovery.EvictionCount))
			}
			if len(discovery.RecentEvictions) > 0 {
				b.WriteString("Recent eviction:\n")
				for _, eviction := range discovery.RecentEvictions {
					b.WriteString(fmt.Sprintf("  %s — %s\n", eviction.Name, eviction.Reason))
				}
			}
			if len(discovery.Recent) > 0 {
				b.WriteString("Recent activation:\n")
				for _, activation := range discovery.Recent {
					b.WriteString(fmt.Sprintf("  %s — %s\n", activation.Name, activation.Reason))
				}
			}
		}
	}

	b.WriteString("\nCumulative Session Token Usage\n")
	if m.stats != nil {
		totalTokens := m.stats.InputTokens + m.stats.CachedInputTokens + m.stats.CacheWriteTokens + m.stats.OutputTokens
		b.WriteString(fmt.Sprintf("Fresh input tokens: %s\n", ui.FormatTokenCount(m.stats.InputTokens)))
		if m.stats.CachedInputTokens > 0 {
			b.WriteString(fmt.Sprintf("Cache read tokens:  %s\n", ui.FormatTokenCount(m.stats.CachedInputTokens)))
		}
		if m.stats.CacheWriteTokens > 0 {
			b.WriteString(fmt.Sprintf("Cache write tokens: %s\n", ui.FormatTokenCount(m.stats.CacheWriteTokens)))
		}
		b.WriteString(fmt.Sprintf("Output tokens:      %s\n", ui.FormatTokenCount(m.stats.OutputTokens)))
		b.WriteString(fmt.Sprintf("Total tokens:       %s\n", ui.FormatTokenCount(totalTokens)))
		inputCategories := m.stats.InputTokens + m.stats.CachedInputTokens + m.stats.CacheWriteTokens
		if inputCategories > 0 {
			b.WriteString(fmt.Sprintf("Cache hit rate:     %.1f%% (cache read / (fresh + read + write input))\n", percent(m.stats.CachedInputTokens, inputCategories)))
		}
		if cost, err := statsCostEstimator(m.statsPricingModel(), m.stats); err == nil {
			b.WriteString(fmt.Sprintf("Estimated cost:     $%.4f\n", cost))
		} else {
			b.WriteString("Estimated cost:     unavailable\n")
		}
	} else {
		b.WriteString("No token usage recorded yet.\n")
	}

	m.renderSubagentStats(&b)

	var sideUsage llm.Usage
	sideRequests := 0
	if m.stats != nil {
		calls, _ := m.stats.UsageCalls()
		for _, call := range calls {
			if !call.SideQuestion {
				continue
			}
			sideRequests++
			sideUsage.Add(llm.Usage{
				InputTokens: call.InputTokens, OutputTokens: call.OutputTokens,
				CachedInputTokens: call.CachedInputTokens, CacheWriteTokens: call.CacheWriteTokens,
			})
		}
	}
	b.WriteString("\nPrivate Side-Question Usage\n")
	b.WriteString(fmt.Sprintf("Requests:           %d\n", sideRequests))
	b.WriteString(fmt.Sprintf("Successful history: %d\n", len(m.sideQuestion.History)))
	b.WriteString(fmt.Sprintf("Fresh input tokens: %s\n", ui.FormatTokenCount(sideUsage.InputTokens)))
	b.WriteString(fmt.Sprintf("Cache read tokens:  %s\n", ui.FormatTokenCount(sideUsage.CachedInputTokens)))
	b.WriteString(fmt.Sprintf("Output tokens:      %s\n", ui.FormatTokenCount(sideUsage.OutputTokens)))

	b.WriteString("\nGuardian Usage\n")
	if m.stats != nil {
		b.WriteString(fmt.Sprintf("Requests:           %d\n", m.stats.GuardianLLMCallCount))
		b.WriteString(fmt.Sprintf("Fresh input tokens: %s\n", ui.FormatTokenCount(m.stats.GuardianInputTokens)))
		b.WriteString(fmt.Sprintf("Cache read tokens:  %s\n", ui.FormatTokenCount(m.stats.GuardianCachedInputTokens)))
		b.WriteString(fmt.Sprintf("Cache write tokens: %s\n", ui.FormatTokenCount(m.stats.GuardianCacheWriteTokens)))
		b.WriteString(fmt.Sprintf("Output tokens:      %s\n", ui.FormatTokenCount(m.stats.GuardianOutputTokens)))
	} else {
		b.WriteString("Requests:           0\n")
	}

	b.WriteString("\nCumulative Session Activity\n")
	if m.stats != nil {
		b.WriteString(fmt.Sprintf("LLM calls:          %d\n", m.stats.LLMCallCount))
		b.WriteString(fmt.Sprintf("Tool calls:         %d\n", m.stats.ToolCallCount))
	}
	if m.sess != nil {
		b.WriteString(fmt.Sprintf("User turns:         %d\n", m.sess.UserTurns))
		b.WriteString(fmt.Sprintf("Assistant turns:    %d\n", m.sess.LLMTurns))
	}
	b.WriteString(fmt.Sprintf("Active messages:    %d (user %d, assistant %d, tool %d)\n", len(activeMessages), roleCounts[string(llm.RoleUser)], roleCounts[string(llm.RoleAssistant)], roleCounts[string(llm.RoleTool)]))

	b.WriteString("\nCompactions\n")
	compactionCount := 0
	compactionSeq := -1
	if m.sess != nil {
		compactionCount = m.sess.CompactionCount
		compactionSeq = m.sess.CompactionSeq
	}
	b.WriteString(fmt.Sprintf("Compactions:        %d\n", compactionCount))
	if m.stats != nil && m.stats.CompactionLLMCallCount > 0 {
		b.WriteString(fmt.Sprintf("LLM cost:           %s\n", formatCompactionUsage(m.stats)))
	}
	if session.HasCompactionBoundary(m.sess) || m.compactionIdx > 0 {
		b.WriteString(fmt.Sprintf("Last boundary:      seq %d (%d messages hidden from active context)\n", compactionSeq, m.compactionIdx))
	} else {
		b.WriteString("Last boundary:      none\n")
	}

	return b.String()
}

func sessionIDOf(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	return sess.ID
}

type compactionAppliedMsg struct {
	generation  uint64
	sessionID   string
	messages    []session.Message
	activeStart int
	refreshed   *session.Session
	model       string
	usage       llm.Usage
}

func (m *Model) recordGuardianUsage(ctx context.Context, model string, u llm.Usage) {
	if u.BillableCountersZero() {
		return
	}
	if m.stats == nil {
		m.stats = ui.NewSessionStats()
	}
	m.stats.AddGuardianUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	sessionID := sessionIDOf(m.sess)
	if m.store != nil && sessionID != "" {
		_ = m.store.UpdateMetrics(ctx, sessionID, 0, 0, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	}
	if m.sess != nil {
		m.sess.InputTokens += u.InputTokens
		m.sess.OutputTokens += u.OutputTokens
		m.sess.CachedInputTokens += u.CachedInputTokens
		m.sess.CacheWriteTokens += u.CacheWriteTokens
	}
}

func (m *Model) recordHandoverUsage(ctx context.Context, sessionID, model string, u llm.Usage) {
	if u.BillableCountersZero() {
		return
	}
	if m.stats == nil {
		m.stats = ui.NewSessionStats()
	}
	m.stats.AddHandoverUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	if m.store != nil && sessionID != "" {
		_ = m.store.UpdateMetrics(ctx, sessionID, 0, 0, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	}
	if m.sess != nil && (sessionID == "" || m.sess.ID == sessionID) {
		m.sess.InputTokens += u.InputTokens
		m.sess.OutputTokens += u.OutputTokens
		m.sess.CachedInputTokens += u.CachedInputTokens
		m.sess.CacheWriteTokens += u.CacheWriteTokens
	}
}

func (m *Model) recordPathNoteUsage(ctx context.Context, sessionID string, u llm.Usage) {
	if u.BillableCountersZero() {
		return
	}
	if m.store != nil && sessionID != "" {
		_ = m.store.UpdateMetrics(ctx, sessionID, 0, 0, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	}
	if m.sess != nil && (sessionID == "" || m.sess.ID == sessionID) {
		m.sess.InputTokens += u.InputTokens
		m.sess.OutputTokens += u.OutputTokens
		m.sess.CachedInputTokens += u.CachedInputTokens
		m.sess.CacheWriteTokens += u.CacheWriteTokens
	}
}

func (m *Model) recordCompactionUsage(ctx context.Context, sessionID, model string, u llm.Usage) {
	if m.stats == nil {
		m.stats = ui.NewSessionStats()
	}
	m.stats.AddCompactionUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	if !u.BillableCountersZero() && m.store != nil && sessionID != "" {
		_ = m.store.UpdateMetrics(ctx, sessionID, 0, 0, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	}
	if !u.BillableCountersZero() && m.sess != nil && (sessionID == "" || m.sess.ID == sessionID) {
		m.sess.InputTokens += u.InputTokens
		m.sess.OutputTokens += u.OutputTokens
		m.sess.CachedInputTokens += u.CachedInputTokens
		m.sess.CacheWriteTokens += u.CacheWriteTokens
	}
}

func formatCompactionUsage(stats *ui.SessionStats) string {
	if stats == nil || stats.CompactionLLMCallCount <= 0 {
		return "none"
	}
	parts := make([]string, 0, 5)
	if stats.CompactionCachedInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s cache", ui.FormatTokenCount(stats.CompactionCachedInputTokens)))
	}
	if stats.CompactionInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s in", ui.FormatTokenCount(stats.CompactionInputTokens)))
	}
	if stats.CompactionCacheWriteTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s write", ui.FormatTokenCount(stats.CompactionCacheWriteTokens)))
	}
	if stats.CompactionOutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s out", ui.FormatTokenCount(stats.CompactionOutputTokens)))
	}
	if len(parts) == 0 {
		parts = append(parts, "usage unknown")
	}
	if stats.CompactionLLMCallCount > 1 {
		parts = append(parts, fmt.Sprintf("%d calls", stats.CompactionLLMCallCount))
	}
	return strings.Join(parts, ", ")
}

func estimateStatsCost(model string, stats *ui.SessionStats) (float64, error) {
	return ui.EstimateSessionStatsCost(stats, model)
}

func (m *Model) statsPricingModel() string {
	model := strings.TrimSpace(m.modelName)
	if model == "" && m.sess != nil {
		model = strings.TrimSpace(m.sess.Model)
	}
	if strings.Contains(model, ":") {
		parts := strings.Split(model, ":")
		model = parts[len(parts)-1]
	}
	if parsedModel, _ := llm.ParseModelEffort(model); strings.TrimSpace(parsedModel) != "" {
		model = strings.TrimSpace(parsedModel)
	}
	if model != "" {
		return model
	}
	provider := strings.TrimSpace(m.providerKey)
	if provider == "" && m.sess != nil {
		provider = strings.TrimSpace(m.sess.ProviderKey)
	}
	if provider != "" && model != "" {
		return provider + ":" + model
	}
	return model
}

func (m *Model) messageSnapshotsForStats() (all []session.Message, active []session.Message) {
	m.messagesMu.Lock()
	defer m.messagesMu.Unlock()
	all = make([]session.Message, len(m.messages))
	copy(all, m.messages)
	start := m.compactionIdx
	if start < 0 || start > len(m.messages) {
		start = 0
	}
	active = make([]session.Message, len(m.messages[start:]))
	copy(active, m.messages[start:])
	return all, active
}

func (m *Model) estimateMessagesTokens(messages []session.Message) int {
	if len(messages) == 0 {
		return 0
	}
	if m.engine != nil {
		llmMessages := make([]llm.Message, 0, len(messages))
		for _, msg := range messages {
			llmMessages = append(llmMessages, msg.ToLLMMessage())
		}
		if tokens := m.engine.EstimateTokens(llmMessages); tokens > 0 {
			return tokens
		}
	}
	total := 0
	for _, msg := range messages {
		total += roughTokenEstimate(msg.TextContent)
	}
	return total
}

func renderContextGrid(used, limit, softThreshold, hardThreshold int) string {
	const cols = 48
	const rows = 6
	total := cols * rows
	usedCells := 0
	softBufferCells := 0
	hardBufferCells := 0
	if limit > 0 {
		usedCells = int(float64(used) / float64(limit) * float64(total))
		if used > 0 && usedCells == 0 {
			usedCells = 1
		}
		if softThreshold > 0 {
			softBufferCells = int(float64(max(0, limit-softThreshold)) / float64(limit) * float64(total))
		}
		if hardThreshold > 0 {
			hardBufferCells = int(float64(max(0, limit-hardThreshold)) / float64(limit) * float64(total))
		}
	}
	var b strings.Builder
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			i := r*cols + c
			switch {
			case i < usedCells:
				b.WriteRune('■')
			case hardBufferCells > 0 && i >= total-hardBufferCells:
				b.WriteRune('!')
			case softBufferCells > 0 && i >= total-softBufferCells:
				b.WriteRune('×')
			default:
				b.WriteRune('□')
			}
		}
		if r < rows-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func percent(part, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(total)
}

func roughTokenEstimate(s string) int {
	if s == "" {
		return 0
	}
	return max(1, len([]rune(s))/4)
}

func nonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "unknown"
}

func subagentStatsCost(calls []ui.SubagentUsageCall) (string, bool, bool) {
	known, priced, unpriced := 0.0, 0, 0
	for _, call := range calls {
		stats := ui.NewSessionStats()
		u := call.Usage
		stats.AddSubagentUsageForModel(call.Model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
		if cost, err := statsCostEstimator("", stats); err == nil {
			known += cost
			priced++
		} else {
			unpriced++
		}
	}
	if priced == 0 {
		return "—", false, true
	}
	cost := fmt.Sprintf("$%.4f", known)
	if unpriced > 0 {
		return "≥" + cost, true, false
	}
	return cost, false, false
}

func subagentStatsLegend(runCount int, running, partial, unavailable bool) string {
	var legend []string
	if running {
		legend = append(legend, "* running")
	}
	if partial {
		legend = append(legend, "≥ partial cost")
	}
	if unavailable {
		legend = append(legend, "— unpriced")
	}
	if runCount > 1 {
		legend = append(legend, "time summed across runs")
	}
	return strings.Join(legend, " · ")
}

// renderSubagentStats reports observed process-local runs separately from restored totals.
func (m *Model) renderSubagentStats(b *strings.Builder) {
	var runs []ui.SubagentProgress
	if m.subagentTracker != nil {
		runs = m.subagentTracker.Snapshots()
	}
	if len(runs) == 0 {
		return
	}
	type modelStats struct {
		model          string
		usage          llm.Usage
		calls          []ui.SubagentUsageCall
		elapsed, tools time.Duration
		timed, running bool
	}
	byModel := map[string]*modelStats{}
	var models []*modelStats
	group := func(model string) *modelStats {
		model = nonEmpty(model, "unknown")
		if found := byModel[model]; found != nil {
			return found
		}
		row := &modelStats{model: model}
		byModel[model] = row
		models = append(models, row)
		return row
	}
	now := time.Now()
	for _, run := range runs {
		row := group(run.ResolvedModel)
		elapsed, tools := run.Timing(now)
		row.elapsed += elapsed
		row.tools += tools
		row.timed = true
		row.running = row.running || !run.Done
		for _, call := range run.UsageCalls {
			owner := group(call.Model)
			owner.usage.Add(call.Usage)
			owner.calls = append(owner.calls, call)
		}
	}
	b.WriteString("\nSubagent models · this process\n")
	width := 96
	if m.dialog != nil {
		width = m.dialog.contentWidth() - 4
	}
	splitTokens := width >= 62
	headers := []string{"Model"}
	widths := []int{32}
	headers = append(headers, "Time", "Tools")
	widths = append(widths, 6, 6)
	if splitTokens {
		headers = append(headers, "In", "Cache", "Out")
		widths = append(widths, 6, 6, 6)
	} else {
		headers = append(headers, "Tokens")
		widths = append(widths, 7)
	}
	headers = append(headers, "Est. cost")
	widths = append(widths, 10)
	fixed := len(widths) - 1
	for _, w := range widths[1:] {
		fixed += w
	}
	widths[0] = max(1, min(widths[0], width-fixed))
	writeRow := func(cells []string) {
		var row strings.Builder
		for i, cell := range cells {
			if i > 0 {
				row.WriteByte(' ')
			}
			cell = ansi.Truncate(terminaltext.SanitizeSingleLine(cell), widths[i], "…")
			pad := strings.Repeat(" ", max(0, widths[i]-ansi.StringWidth(cell)))
			if i == 0 {
				row.WriteString(cell + pad)
			} else {
				row.WriteString(pad + cell)
			}
		}
		b.WriteString(strings.TrimRight(ansi.Truncate(row.String(), max(1, width), "…"), " ") + "\n")
	}
	writeRow(headers)
	partial, running, unavailable := false, false, false
	for _, row := range models {
		name := row.model
		if row.running {
			name = ansi.Truncate(name, max(1, widths[0]-1), "…") + "*"
			running = true
		}
		cells := []string{name}
		if row.timed {
			cells = append(cells, fmt.Sprintf("%.1fs", row.elapsed.Seconds()), fmt.Sprintf("%.1fs", row.tools.Seconds()))
		} else {
			cells = append(cells, "—", "—")
		}
		u := row.usage
		if splitTokens {
			cells = append(cells, ui.FormatTokenCount(u.InputTokens), ui.FormatTokenCount(u.CachedInputTokens+u.CacheWriteTokens), ui.FormatTokenCount(u.OutputTokens))
		} else {
			cells = append(cells, ui.FormatTokenCount(u.InputTokens+u.CachedInputTokens+u.CacheWriteTokens+u.OutputTokens))
		}
		cost, costPartial, costUnavailable := subagentStatsCost(row.calls)
		partial = partial || costPartial
		unavailable = unavailable || costUnavailable
		writeRow(append(cells, cost))
	}
	if legend := subagentStatsLegend(len(runs), running, partial, unavailable); legend != "" {
		b.WriteString(legend + "\n")
	}
}

func (m *Model) recordSubagentUsage(ctx context.Context, model string, u llm.Usage) {
	if u.BillableCountersZero() {
		return
	}
	if m.stats == nil {
		m.stats = ui.NewSessionStats()
	}
	m.stats.AddSubagentUsageForModel(model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	sessionID := sessionIDOf(m.sess)
	if m.store != nil && sessionID != "" {
		_ = m.store.UpdateMetrics(ctx, sessionID, 0, 0, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	}
	if m.sess != nil {
		m.sess.InputTokens += u.InputTokens
		m.sess.OutputTokens += u.OutputTokens
		m.sess.CachedInputTokens += u.CachedInputTokens
		m.sess.CacheWriteTokens += u.CacheWriteTokens
	}
}

// recordChildEventUsage shares parent accounting between spawned agents and isolated skills.
func (m *Model) recordChildEventUsage(callID string, event tools.SubagentEvent) {
	switch event.Type {
	case tools.SubagentEventUsage:
		model := event.Model
		if model == "" && m.subagentTracker != nil {
			model = m.subagentTracker.ResolvedModel(callID)
		}
		m.recordSubagentUsage(context.Background(), model, llm.Usage{InputTokens: event.InputTokens, OutputTokens: event.OutputTokens, CachedInputTokens: event.CachedInputTokens, CacheWriteTokens: event.CacheWriteTokens})
	case tools.SubagentEventGuardian:
		if event.Guardian != nil {
			m.recordGuardianUsage(context.Background(), event.Guardian.Model, event.Guardian.Usage)
		}
	}
}
