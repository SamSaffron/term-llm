package chat

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/ui"
)

type reasoningClickTargetKind int

const (
	reasoningClickTargetHistory reasoningClickTargetKind = iota
	reasoningClickTargetTracker
	reasoningClickTargetCurrent
)

type reasoningClickTarget struct {
	kind                reasoningClickTargetKind
	historyOrdinal      int
	trackerSegmentIndex int
}

type reasoningClickSnapshot struct {
	historyLineOrdinals       map[int]int
	trackerSegmentIndexByLine map[int]int
	currentReasoningLine      int
	hasCurrentReasoningLine   bool
}

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

func (m *Model) reasoningClickTargetAtLine(line int) (reasoningClickTarget, bool) {
	snapshot := m.viewCache.reasoningClickSnapshot
	if ordinal, ok := snapshot.historyLineOrdinals[line]; ok {
		return reasoningClickTarget{kind: reasoningClickTargetHistory, historyOrdinal: ordinal}, true
	}
	if segmentIndex, ok := snapshot.trackerSegmentIndexByLine[line]; ok {
		return reasoningClickTarget{kind: reasoningClickTargetTracker, trackerSegmentIndex: segmentIndex}, true
	}
	if snapshot.hasCurrentReasoningLine && snapshot.currentReasoningLine == line {
		return reasoningClickTarget{kind: reasoningClickTargetCurrent}, true
	}
	return reasoningClickTarget{}, false
}

func (m *Model) toggleReasoningSegmentAtContentLine(line int) bool {
	target, ok := m.reasoningClickTargetAtLine(line)
	if !ok {
		return false
	}
	switch target.kind {
	case reasoningClickTargetHistory:
		return m.toggleHistoryReasoningOrdinal(target.historyOrdinal)
	case reasoningClickTargetTracker:
		return m.toggleTrackerReasoningSegment(target.trackerSegmentIndex)
	case reasoningClickTargetCurrent:
		return m.toggleCurrentReasoningBlock()
	default:
		return false
	}
}

func (m *Model) toggleCurrentReasoningBlock() bool {
	part, ok := m.currentReasoningDisplayPart()
	if !ok {
		return false
	}
	current := false
	if m.currentReasoningExpanded != nil {
		current = *m.currentReasoningExpanded
	} else {
		kind := llm.NormalizeReasoningKind(part.ReasoningKind)
		current = internalreasoning.HistoryExpanded(string(kind), m.effectiveReasoningConfig())
	}
	next := !current
	m.currentReasoningExpanded = &next
	m.viewCache.cachedCompletedContent = ""
	m.viewCache.cachedTrackerVersion = 0
	m.viewCache.lastTrackerVersion = 0
	m.viewCache.lastSetContentAt = time.Time{}
	m.resetAltScreenStreamingAppendCache()
	m.bumpContentVersion()
	return true
}

func (m *Model) toggleTrackerReasoningSegment(segmentIndex int) bool {
	if m.tracker == nil || segmentIndex < 0 || segmentIndex >= len(m.tracker.Segments) {
		return false
	}
	seg := &m.tracker.Segments[segmentIndex]
	if seg.Reasoning == nil {
		return false
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

func renderedHistoryLineCount(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n")
}

func (m *Model) captureReasoningClickSnapshot() {
	snapshot := reasoningClickSnapshot{}
	if m.chatRenderer != nil {
		snapshot.historyLineOrdinals = m.chatRenderer.ReasoningLineOrdinalsSnapshot()
	}
	historyLines := renderedHistoryLineCount(m.viewCache.historyContent)
	snapshot.trackerSegmentIndexByLine = m.trackerReasoningLineSnapshot(historyLines)
	if line, ok := m.currentReasoningLineSnapshot(historyLines); ok {
		snapshot.currentReasoningLine = line
		snapshot.hasCurrentReasoningLine = true
	}
	m.viewCache.reasoningClickSnapshot = snapshot
}

func (m *Model) trackerReasoningLineSnapshot(historyLines int) map[int]int {
	if m.tracker == nil {
		return nil
	}
	lineBySegment := make(map[int]int)
	runningLines := 0
	lastType := ui.SegmentText
	hasPrev := false
	leadingInitialized := false
	trackerExpanded := m.tracker.Expanded()
	for i := range m.tracker.Segments {
		seg := &m.tracker.Segments[i]
		if seg.Flushed {
			continue
		}
		if seg.Type == ui.SegmentTool && seg.ToolStatus == ui.ToolPending {
			break
		}
		if !leadingInitialized {
			if m.tracker.HasFlushed && seg.FlushedPos == 0 {
				lastType = m.tracker.LastFlushedType
				hasPrev = true
			}
			leadingInitialized = true
		}
		rendered := ui.RenderSegmentsWithImageRenderer([]*ui.Segment{seg}, m.width, -1, m.renderMd, m.altScreen, trackerExpanded, m.imageArtifactRenderer())
		if rendered == "" {
			continue
		}
		if hasPrev {
			runningLines += strings.Count(ui.SegmentSeparator(lastType, seg.Type), "\n")
		}
		if seg.Reasoning != nil && seg.Rendered != "" {
			lineBySegment[historyLines+runningLines] = i
		}
		runningLines += strings.Count(rendered, "\n")
		lastType = seg.Type
		hasPrev = true
	}
	if len(lineBySegment) == 0 {
		return nil
	}
	return lineBySegment
}

func (m *Model) currentReasoningLineSnapshot(historyLines int) (int, bool) {
	if !m.streaming {
		return 0, false
	}
	activeReasoning := m.renderCurrentReasoningBlock()
	if activeReasoning == "" {
		return 0, false
	}
	completedContent := ""
	if m.tracker != nil {
		completedContent = m.viewCache.cachedCompletedContent
		if m.viewCache.cachedTrackerVersion != m.tracker.Version {
			completedContent = m.tracker.RenderUnflushedWithImageRenderer(m.width, m.renderMd, m.altScreen, m.imageArtifactRenderer())
		}
	}
	runningLines := strings.Count(completedContent, "\n")
	if completedContent != "" {
		if !strings.HasSuffix(completedContent, "\n\n") {
			if strings.HasSuffix(completedContent, "\n") {
				runningLines++
			} else {
				runningLines += 2
			}
		}
	} else if m.tracker != nil && m.tracker.HasFlushed {
		runningLines += strings.Count(m.tracker.FlushLeadingSeparator(ui.SegmentReasoning), "\n")
	}
	return historyLines + runningLines, true
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
	m.currentReasoningExpanded = nil
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
	// A per-block override must be authoritative for that block, so set both
	// Display and History: the renderer's HistoryExpanded() treats History ==
	// expanded as expanded regardless of Display, so changing Display alone
	// would leave a collapse override as a no-op under reasoning.history:
	// expanded.
	if *expanded {
		cfg.Display = config.ReasoningDisplayExpanded
		cfg.History = config.ReasoningHistoryExpanded
		return cfg
	}
	cfg.Display = config.ReasoningDisplayCollapsed
	cfg.History = config.ReasoningHistoryCollapsed
	return cfg
}
