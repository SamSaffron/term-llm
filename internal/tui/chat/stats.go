package chat

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
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
	snapshot := *m.stats
	snapshot.ClearEstimatedCost()
	if cost, err := statsCostEstimator(m.statsPricingModel(), &snapshot); err == nil {
		snapshot.SetEstimatedCost(cost)
	}
	return snapshot.Render()
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

type compactionUsageMsg struct {
	sessionID string
	model     string
	usage     llm.Usage
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
