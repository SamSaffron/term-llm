package chat

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m *Model) handleReasoningMouseClick(msg tea.MouseMsg) bool {
	click, ok := msg.(tea.MouseClickMsg)
	if !ok {
		return false
	}
	if click.Button != tea.MouseLeft {
		return false
	}
	if !m.altScreen {
		return false
	}
	if !m.isInViewportArea(click.X, click.Y) {
		return false
	}
	contentLine, _ := m.screenToContent(click.X, click.Y)
	return m.toggleReasoningSegmentAtContentLine(contentLine)
}

func (m *Model) contentLineText(line int) string {
	if m.contentLines == nil && m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}
	if line < 0 || line >= len(m.contentLines) {
		return ""
	}
	return m.contentLines[line]
}

func isReasoningHeaderLine(line string, cfg config.ReasoningConfig) bool {
	plain := strings.TrimSpace(ansi.Strip(line))
	if strings.HasPrefix(plain, "▸ ") {
		plain = strings.TrimPrefix(plain, "▸ ")
	} else if strings.HasPrefix(plain, "▾ ") {
		plain = strings.TrimPrefix(plain, "▾ ")
	} else {
		return false
	}
	if strings.HasPrefix(plain, "Thought: ") {
		return true
	}
	label := strings.TrimSpace(cfg.HiddenLabel)
	if label == "" {
		label = config.DefaultReasoningConfig().HiddenLabel
	}
	return plain == label
}

func (m *Model) reasoningHeaderOrdinalAtLine(line int) (int, bool) {
	historyLines := 0
	historyReasoningCount := 0
	if m.chatRenderer != nil {
		if ordinal, ok := m.chatRenderer.ReasoningOrdinalAtLine(line); ok {
			return ordinal, true
		}
		historyLines = renderedHistoryLineCount(m.viewCache.historyContent)
		historyReasoningCount = m.chatRenderer.ReasoningHeaderCount()
		if line < historyLines {
			return 0, false
		}
	}

	cfg := m.effectiveReasoningConfig()
	lineText := m.contentLineText(line)
	if !isReasoningHeaderLine(lineText, cfg) {
		return 0, false
	}
	ordinal := historyReasoningCount
	for i := historyLines; i < line; i++ {
		if isReasoningHeaderLine(m.contentLineText(i), cfg) {
			ordinal++
		}
	}
	return ordinal, true
}

func (m *Model) toggleReasoningSegmentAtContentLine(line int) bool {
	ordinal, ok := m.reasoningHeaderOrdinalAtLine(line)
	if !ok {
		return false
	}
	historyCount := m.renderedHistoryReasoningCount()
	if ordinal < historyCount {
		return m.toggleHistoryReasoningOrdinal(ordinal)
	}
	return m.toggleTrackerReasoningOrdinal(ordinal - historyCount)
}

func (m *Model) toggleTrackerReasoningOrdinal(ordinal int) bool {
	if m.tracker == nil {
		return false
	}
	seen := 0
	for i := range m.tracker.Segments {
		seg := &m.tracker.Segments[i]
		if seg.Reasoning == nil {
			continue
		}
		if seen != ordinal {
			seen++
			continue
		}
		currentlyExpanded := m.reasoningSegmentExpanded(*seg.Reasoning)
		next := !currentlyExpanded
		seg.Reasoning.Expanded = &next
		rendered := ui.NormalizeReasoningSegmentRendered(m.renderReasoningSegmentBlock(*seg.Reasoning))
		if rendered == "" {
			return false
		}
		seg.Text = rendered
		seg.Rendered = rendered
		m.tracker.Version++
		m.viewCache.cachedCompletedContent = ""
		m.viewCache.cachedTrackerVersion = 0
		m.viewCache.lastTrackerVersion = 0
		m.viewCache.lastSetContentAt = time.Time{}
		m.resetAltScreenStreamingAppendCache()
		m.rerenderCompletedStreamFromTracker()
		m.bumpContentVersion()
		return true
	}
	return false
}

func renderedHistoryLineCount(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n")
}

func (m *Model) renderedHistoryReasoningCount() int {
	if m.chatRenderer != nil {
		return m.chatRenderer.ReasoningHeaderCount()
	}
	if m.viewCache.historyContent != "" {
		cfg := m.effectiveReasoningConfig()
		count := 0
		for _, line := range strings.Split(m.viewCache.historyContent, "\n") {
			if isReasoningHeaderLine(line, cfg) {
				count++
			}
		}
		return count
	}
	return 0
}

func (m *Model) forceHistoryRerender() {
	m.forceHistoryRerenderWithCacheInvalidation(true)
}

func (m *Model) forceHistoryRerenderPreservingBlockCache() {
	m.forceHistoryRerenderWithCacheInvalidation(false)
}

func (m *Model) forceHistoryRerenderWithCacheInvalidation(invalidateBlockCache bool) {
	m.viewCache.historyValid = false
	m.viewCache.historySignature = 0
	m.viewCache.historyLines = nil
	m.resetAltScreenStreamingAppendCache()
	if invalidateBlockCache && m.chatRenderer != nil {
		m.chatRenderer.InvalidateCache()
	}
	m.viewCache.lastSetContentAt = time.Time{}
	m.bumpContentVersion()
}

func (m *Model) toggleHistoryReasoningOrdinal(ordinal int) bool {
	if m.reasoningExpansionOverrides == nil {
		m.reasoningExpansionOverrides = make(map[int]bool)
	}
	defaultExpanded := internalreasoning.HistoryExpanded(string(llm.ReasoningKindRaw), m.effectiveReasoningConfig())
	current := defaultExpanded
	if v, ok := m.reasoningExpansionOverrides[ordinal]; ok {
		current = v
	}
	next := !current
	if next == defaultExpanded {
		delete(m.reasoningExpansionOverrides, ordinal)
		if len(m.reasoningExpansionOverrides) == 0 {
			m.reasoningExpansionOverrides = nil
		}
	} else {
		m.reasoningExpansionOverrides[ordinal] = next
	}
	m.forceHistoryRerenderPreservingBlockCache()
	return true
}

func (m *Model) reasoningSegmentExpanded(seg ui.ReasoningSegment) bool {
	if seg.Expanded != nil {
		return *seg.Expanded
	}
	kind := llm.NormalizeReasoningKind(llm.ReasoningKind(seg.Kind))
	return internalreasoning.HistoryExpanded(string(kind), m.effectiveReasoningConfig())
}

func (m *Model) clearReasoningSegmentExpansionOverrides() {
	m.reasoningExpansionOverrides = nil
	if m.tracker == nil {
		return
	}
	for i := range m.tracker.Segments {
		if m.tracker.Segments[i].Reasoning != nil {
			m.tracker.Segments[i].Reasoning.Expanded = nil
		}
	}
}

func reasoningConfigWithSegmentExpansion(cfg config.ReasoningConfig, expanded *bool) config.ReasoningConfig {
	if expanded == nil {
		return cfg
	}
	if *expanded {
		cfg.Display = config.ReasoningDisplayExpanded
		return cfg
	}
	cfg.Display = config.ReasoningDisplayCollapsed
	return cfg
}
