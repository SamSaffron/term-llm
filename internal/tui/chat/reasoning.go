package chat

import (
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	renderchat "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m *Model) effectiveReasoningConfig() config.ReasoningConfig {
	cfg := m.reasoningConfig
	if cfg.Display == "" {
		cfg = config.DefaultReasoningConfig()
		if m.config != nil {
			cfg = m.config.ResolveReasoning("chat")
		}
	}
	if m.reasoningModeOverride != "" {
		cfg.Display = m.reasoningModeOverride
	}
	return cfg
}

func (m *Model) resetCurrentReasoning() {
	m.resetActiveReasoning()
	m.committedReasoning = nil
}

func (m *Model) resetActiveReasoning() {
	m.currentReasoning.Reset()
	m.currentReasoningItemID = ""
	m.currentReasoningKind = ""
	m.currentReasoningTitle = ""
	m.currentReasoningExpanded = nil
	m.reasoningPhaseActive = false
}

func (m *Model) mergeCurrentReasoningKind(kind llm.ReasoningKind) {
	m.currentReasoningKind = llm.MergeReasoningKind(m.currentReasoningKind, kind)
}

func (m *Model) handleReasoningStreamEvent(ev ui.StreamEvent) {
	cfg := m.effectiveReasoningConfig()
	kind := llm.NormalizeReasoningKind(ev.ReasoningKind)
	if !internalreasoning.IsDisplayable(string(kind), cfg) {
		return
	}
	internalreasoning.AppendStreamItemText(&m.currentReasoning, &m.currentReasoningItemID, ev.ReasoningText, ev.ReasoningItemID)
	if ev.ReasoningText != "" {
		m.mergeCurrentReasoningKind(kind)
		limited := internalreasoning.LimitReasoningText(string(kind), m.currentReasoning.String(), cfg)
		if limited != m.currentReasoning.String() {
			m.currentReasoning.Reset()
			m.currentReasoning.WriteString(limited)
		}
	}

	title := strings.TrimSpace(ev.ReasoningTitle)
	if title == "" && kind == llm.ReasoningKindSummary {
		title = internalreasoning.SummaryTitle(m.currentReasoning.String(), cfg)
	}
	if title == "" && kind == llm.ReasoningKindRaw && internalreasoning.EffectiveDisplay(cfg) == config.ReasoningDisplayRaw {
		title = "Raw thinking"
	}
	if title != "" {
		m.currentReasoningTitle = title
	}
	m.bumpContentVersion()
	m.applyReasoningPhase()
}

func (m *Model) applyReasoningPhase() {
	cfg := m.effectiveReasoningConfig()
	if !internalreasoning.StatusEnabled(cfg) {
		if m.reasoningPhaseActive {
			m.phase = "Thinking"
			m.reasoningPhaseActive = false
		}
		return
	}
	if m.retryStatus != "" {
		return
	}
	if m.tracker != nil && m.tracker.HasPending() {
		return
	}
	phase := strings.TrimSpace(m.phase)
	if phase != "" && phase != "Thinking" && !strings.HasPrefix(phase, "Thinking:") && !m.reasoningPhaseActive {
		return
	}

	statusMode := strings.ToLower(strings.TrimSpace(cfg.Status))
	title := strings.TrimSpace(m.currentReasoningTitle)
	if title == "" && statusMode == config.ReasoningStatusSummary {
		summary := strings.Join(strings.Fields(internalreasoning.SummaryBody(m.currentReasoning.String(), cfg)), " ")
		title = internalreasoning.TruncateRunes(summary, internalreasoning.MaxReasoningTitleRunes)
	}
	if title == "" {
		if statusMode == config.ReasoningStatusGeneric {
			m.phase = "Thinking"
			m.reasoningPhaseActive = false
		}
		return
	}
	// Reasoning-derived status titles are already scoped by the spinner/status UI.
	// Keep the line compact by showing the action directly instead of "Thinking: …".
	m.phase = title
	m.reasoningPhaseActive = true
}

func (m *Model) activeReasoningPartMetadata() (content string, kind llm.ReasoningKind, title string) {
	cfg := m.effectiveReasoningConfig()
	kind = llm.NormalizeReasoningKind(m.currentReasoningKind)
	if m.currentReasoning.Len() == 0 || !internalreasoning.HistoryVisible(string(kind), cfg) {
		return "", "", ""
	}
	content = internalreasoning.LimitReasoningText(string(kind), m.currentReasoning.String(), cfg)
	title = strings.TrimSpace(m.currentReasoningTitle)
	if title == "" && kind == llm.ReasoningKindSummary {
		title = internalreasoning.SummaryTitle(content, cfg)
	}
	return content, kind, title
}

// currentReasoningPartMetadata returns reasoning metadata for the assistant
// message currently being persisted: committed reasoning blocks from earlier
// stream boundaries plus the active in-progress reasoning buffer. By contrast,
// activeReasoningPartMetadata only describes the live buffer that is currently
// visible in the streaming UI.
func (m *Model) currentReasoningPartMetadata() (content string, kind llm.ReasoningKind, title string) {
	var parts []llm.Part
	parts = append(parts, m.committedReasoning...)
	if active, ok := m.currentReasoningDisplayPart(); ok {
		parts = append(parts, active)
	}
	if len(parts) == 0 {
		return "", "", ""
	}
	kind = llm.NormalizeReasoningKind(parts[0].ReasoningKind)
	for i, part := range parts {
		partKind := llm.NormalizeReasoningKind(part.ReasoningKind)
		if i == 0 {
			kind = partKind
		} else if kind != partKind {
			if kind == llm.ReasoningKindRaw || partKind == llm.ReasoningKindRaw {
				kind = llm.ReasoningKindRaw
			} else {
				kind = llm.ReasoningKindUnknown
			}
		}
		text := strings.TrimSpace(part.ReasoningContent)
		if text == "" {
			continue
		}
		if content != "" {
			content += "\n\n"
		}
		content += text
		if title == "" {
			title = strings.TrimSpace(part.ReasoningSummaryTitle)
		}
	}
	if content == "" {
		return "", "", ""
	}
	return content, kind, title
}

func (m *Model) currentReasoningDisplayPart() (llm.Part, bool) {
	content, kind, title := m.activeReasoningPartMetadata()
	if content == "" {
		return llm.Part{}, false
	}
	return llm.Part{
		Type:                  llm.PartText,
		ReasoningContent:      content,
		ReasoningKind:         kind,
		ReasoningSummaryTitle: title,
	}, true
}

func reasoningSegmentFromPart(part llm.Part) ui.ReasoningSegment {
	return ui.ReasoningSegment{
		Content: part.ReasoningContent,
		Kind:    string(part.ReasoningKind),
		Title:   part.ReasoningSummaryTitle,
	}
}

func partFromReasoningSegment(seg ui.ReasoningSegment) llm.Part {
	return llm.Part{
		Type:                  llm.PartText,
		ReasoningContent:      seg.Content,
		ReasoningKind:         llm.ReasoningKind(seg.Kind),
		ReasoningSummaryTitle: seg.Title,
	}
}

func (m *Model) renderReasoningSegmentBlock(seg ui.ReasoningSegment) string {
	return m.renderReasoningPartBlockWithConfig(partFromReasoningSegment(seg), reasoningConfigWithSegmentExpansion(m.effectiveReasoningConfig(), seg.Expanded))
}

func (m *Model) renderReasoningPartBlock(part llm.Part) string {
	return m.renderReasoningPartBlockWithConfig(part, m.effectiveReasoningConfig())
}

func (m *Model) renderReasoningPartBlockWithConfig(part llm.Part, cfg config.ReasoningConfig) string {
	r := renderchat.NewMessageBlockRenderer(m.width, m.renderMd, m.toolsExpanded)
	r.SetReasoningConfig(cfg)
	block := r.Render(&session.Message{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{part},
	})
	if block == nil {
		return ""
	}
	return block.Rendered
}

func (m *Model) renderCurrentReasoningBlock() string {
	part, ok := m.currentReasoningDisplayPart()
	if !ok {
		return ""
	}
	return m.renderReasoningPartBlockWithConfig(part, reasoningConfigWithSegmentExpansion(m.effectiveReasoningConfig(), m.currentReasoningExpanded))
}

func (m *Model) rerenderCommittedReasoningSegments() {
	if m.tracker == nil {
		return
	}
	changed := false
	for i := range m.tracker.Segments {
		reasoning := m.tracker.Segments[i].Reasoning
		if reasoning == nil {
			continue
		}
		rendered := ui.NormalizeReasoningSegmentRendered(m.renderReasoningSegmentBlock(*reasoning))
		if rendered == "" {
			continue
		}
		m.tracker.Segments[i].Text = rendered
		m.tracker.Segments[i].Rendered = rendered
		changed = true
	}
	if changed {
		m.tracker.Version++
		m.viewCache.cachedCompletedContent = ""
		m.viewCache.cachedTrackerVersion = 0
		m.viewCache.lastTrackerVersion = 0
		m.bumpContentVersion()
	}
}

func (m *Model) commitCurrentReasoningToStream() {
	part, ok := m.currentReasoningDisplayPart()
	if !ok || m.tracker == nil {
		return
	}
	rendered := m.renderCurrentReasoningBlock()
	if strings.TrimSpace(rendered) == "" {
		return
	}
	seg := reasoningSegmentFromPart(part)
	seg.Expanded = m.currentReasoningExpanded
	m.tracker.AddReasoningSegment(rendered, seg)
	m.committedReasoning = append(m.committedReasoning, part)
	m.resetActiveReasoning()
	m.viewCache.cachedCompletedContent = ""
	m.viewCache.cachedTrackerVersion = 0
	m.bumpContentVersion()
}

func (m *Model) rerenderCompletedStreamFromTracker() {
	if !m.altScreen || m.streaming || m.tracker == nil || m.viewCache.completedStream == "" {
		return
	}
	completed := m.tracker.CompletedSegments()
	if len(completed) == 0 {
		return
	}
	m.resetAltScreenStreamingAppendCache()
	m.viewCache.completedStream = ui.RenderSegmentsWithImageRenderer(completed, m.width, -1, m.renderMd, true, m.toolsExpanded, m.imageArtifactRenderer())
	m.viewCache.lastSetContentAt = time.Time{}
	m.bumpContentVersion()
}

func (m *Model) setReasoningDetailsExpanded(expanded bool) {
	cfg := m.effectiveReasoningConfig()
	display := internalreasoning.EffectiveDisplay(cfg)
	switch display {
	case config.ReasoningDisplayCollapsed, config.ReasoningDisplayExpanded:
		if expanded {
			m.reasoningModeOverride = config.ReasoningDisplayExpanded
		} else {
			m.reasoningModeOverride = config.ReasoningDisplayCollapsed
		}
	default:
		// Off/status/raw have explicit semantics outside the ordinary
		// collapsed/expanded thought-detail toggle. Tool expansion is handled
		// independently by the key handler.
		return
	}
	m.clearReasoningSegmentExpansionOverrides()
	if m.chatRenderer != nil {
		m.chatRenderer.SetReasoningConfig(m.effectiveReasoningConfig())
	}
	m.rerenderCommittedReasoningSegments()
	m.forceHistoryRerender()
	m.applyReasoningPhase()
}
