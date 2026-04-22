package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	render "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// maxViewLines is the maximum number of lines to keep in View().
// Content beyond this is printed to scrollback to prevent scroll issues.
const maxViewLines = 8

// View renders the model
func (m *Model) View() tea.View {
	if m.quitting {
		return m.newView("")
	}
	m.textareaBoundsValid = false

	// Inspector mode uses alternate screen
	if m.inspectorMode && m.inspectorModel != nil {
		return m.inspectorModel.View()
	}

	// Resume browser mode uses the dedicated sessions browser view
	if m.resumeBrowserMode && m.resumeBrowserModel != nil {
		return m.resumeBrowserModel.View()
	}

	if m.streaming && m.streamPerf != nil {
		m.streamPerf.RecordFrameAt(time.Now())
	}

	// Set terminal title
	title := m.getTerminalTitle()
	titleSeq := fmt.Sprintf("\x1b]0;%s\x07", title)

	// Alt screen mode: use viewport for scrollable content
	if m.altScreen {
		return m.newView(titleSeq + m.viewAltScreen())
	}

	// Auto-send mode: minimal rendering for benchmarking (skip expensive UI)
	if m.autoSendQueue != nil {
		return m.newView(titleSeq + m.viewAutoSend())
	}

	// Inline mode: traditional rendering
	var b strings.Builder
	renderedLines := 0

	// History (if scrolling)
	if m.scrollOffset > 0 {
		history := m.renderHistory()
		b.WriteString(history)
		renderedLines += lipgloss.Height(history)
		b.WriteString("\n")
		renderedLines++
	}

	// Streaming response (if active)
	if m.streaming {
		streaming := m.renderStreamingInline()
		b.WriteString(streaming)
		renderedLines += lipgloss.Height(streaming)
	}

	// Error display (if error occurred and not streaming)
	if m.err != nil && !m.streaming {
		errOutput := m.renderError()
		b.WriteString(errOutput)
		renderedLines += lipgloss.Height(errOutput)
		b.WriteString("\n\n")
		renderedLines += 2
	}

	// Completions popup (if visible)
	if m.completions.IsVisible() {
		completions := m.completions.View()
		b.WriteString(completions)
		renderedLines += lipgloss.Height(completions)
		b.WriteString("\n")
		renderedLines++
	}

	// Dialog (if open)
	if m.dialog.IsOpen() {
		dialog := m.dialog.View()
		b.WriteString(dialog)
		renderedLines += lipgloss.Height(dialog)
		b.WriteString("\n")
		renderedLines++
	}

	footer := m.buildFooterLayout()
	m.applyFooterLayout(renderedLines, footer)
	b.WriteString(footer.view)

	return m.newView(titleSeq + b.String())
}

// newView wraps content in a tea.View with the model's declarative flags.
func (m *Model) newView(content string) tea.View {
	v := tea.NewView(content)
	if m.altScreen {
		v.AltScreen = true
	}
	if m.autoSendQueue == nil {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// viewAltScreen renders the full-screen alt screen view with scrollable viewport
func (m *Model) viewAltScreen() string {
	var b strings.Builder
	renderedLines := 0
	footer := m.buildFooterLayout()
	m.syncAltScreenViewportHeight(footer.height)

	// Build scrollable content with caching to avoid re-rendering unchanged content

	// Check if history cache is valid.
	// Skip expensive signature computation when the cache is already valid and
	// dimensions/scroll haven't changed — this eliminates O(total_content_bytes)
	// hashing on every frame during streaming.
	historyValid := m.viewCache.historyValid &&
		m.viewCache.historyWidth == m.width &&
		m.viewCache.historyScrollOffset == m.scrollOffset
	if !historyValid {
		historySig := render.MessageHistorySignature(m.messages)
		if m.viewCache.historyValid && m.viewCache.historySignature == historySig && m.viewCache.historyWidth == m.width {
			// Content unchanged despite invalidation (e.g. scroll offset change) — restore validity.
			m.viewCache.historyScrollOffset = m.scrollOffset
		} else {
			m.viewCache.historyContent = m.renderHistory()
			m.viewCache.historyMsgCount = len(m.messages)
			m.viewCache.historySignature = historySig
			m.viewCache.historyWidth = m.width
			m.viewCache.historyScrollOffset = m.scrollOffset
			m.viewCache.historyValid = true
			m.bumpContentVersion() // History changed
		}
	}

	// Track whether we need to rebuild viewport content this frame.
	// When throttled, we intentionally skip expensive content reconstruction and
	// keep the previous viewport content until the next render tick.
	var contentStr string
	if m.streaming {
		// Only increment version when tracker content or wave position changes
		// The completed segment rendering is cached, but we still need SetContent
		// for wave animation updates
		if m.tracker != nil {
			trackerVersion := m.tracker.Version
			if m.streamPerf != nil {
				m.streamPerf.RecordTrackerVersion(trackerVersion)
			}
			wavePos := m.tracker.WavePos
			contentChanged := trackerVersion != m.viewCache.lastTrackerVersion
			waveChanged := wavePos != m.viewCache.lastWavePos && m.tracker.HasPending()
			if contentChanged || waveChanged {
				m.viewCache.lastTrackerVersion = trackerVersion
				m.viewCache.lastWavePos = wavePos
				m.bumpContentVersion()
			}
		}
	}

	// Only call SetContent if content actually changed (expensive operation)
	// Use version comparison instead of O(n) string comparison
	contentChanged := m.viewCache.contentVersion != m.viewCache.lastRenderedVersion

	// Force update if embedded UI is active (since it's interactive and doesn't affect tracker version)
	if m.approvalModel != nil || m.askUserModel != nil || m.handoverPreview != nil {
		contentChanged = true
	}
	if contentChanged {
		now := time.Now()
		if m.shouldThrottleSetContent(now) {
			contentChanged = false
			if m.streamPerf != nil {
				m.streamPerf.tracef("set_content throttled")
			}
		}
	}

	if contentChanged {
		if m.streaming {
			streamingContent := m.renderStreamingInline()
			contentStr = m.viewCache.historyContent + streamingContent
			if m.approvalModel != nil {
				contentStr += "\n" + m.approvalModel.View().Content
			} else if m.askUserModel != nil {
				contentStr += "\n" + m.askUserModel.View().Content
			} else if m.handoverPreview != nil {
				contentStr += m.handoverPreview.View()
			}
		} else {
			contentStr = m.viewCache.historyContent + m.viewCache.completedStream
			if m.handoverPreview != nil {
				contentStr += m.handoverPreview.View()
			}
			if m.err != nil {
				contentStr += "\n" + m.renderError() + "\n"
			}
		}

		// Check if user is at bottom BEFORE setting content (which changes maxYOffset)
		wasAtBottom := m.viewport.AtBottom()
		firstRender := m.viewCache.lastViewportView == ""
		setContentStart := time.Now()
		m.viewport.SetContent(contentStr)
		setContentEnd := time.Now()
		if m.streamPerf != nil {
			m.streamPerf.RecordDuration(durationMetricSetContent, setContentEnd.Sub(setContentStart))
		}
		m.viewCache.lastSetContentAt = setContentEnd
		m.viewCache.lastRenderedVersion = m.viewCache.contentVersion
		// On first render (including resumed sessions), anchor at latest content.
		// On subsequent renders while streaming, preserve user scroll position
		// unless they were already at bottom.
		if m.handoverPreview != nil && m.handoverPreview.editing {
			m.scrollToBottom = true
		}
		if firstRender || (m.streaming && wasAtBottom) || m.scrollToBottom {
			m.viewport.GotoBottom()
			m.scrollToBottom = false
		}
	}

	// Scroll to bottom after response completes (regardless of previous scroll position)
	if m.scrollToBottom {
		m.viewport.GotoBottom()
		m.scrollToBottom = false
	}

	// Cache viewport.View() output - only regenerate if content, scroll position, or size changed
	// Check YOffset after GotoBottom() since it modifies the offset
	yOffsetChanged := m.viewport.YOffset() != m.viewCache.lastYOffset
	sizeChanged := m.viewport.Width() != m.viewCache.lastVPWidth || m.viewport.Height() != m.viewCache.lastVPHeight

	// Force re-render when selection changes
	selectionChanged := m.selection != m.viewCache.lastSelection
	needViewRender := contentChanged || yOffsetChanged || sizeChanged || selectionChanged || m.viewCache.lastViewportView == ""
	if needViewRender {
		viewStart := time.Now()
		m.viewCache.lastViewportView = m.viewport.View()
		if m.streamPerf != nil {
			m.streamPerf.RecordDuration(durationMetricViewportView, time.Since(viewStart))
		}
		m.viewCache.lastYOffset = m.viewport.YOffset()
		m.viewCache.lastVPWidth = m.viewport.Width()
		m.viewCache.lastVPHeight = m.viewport.Height()
		m.viewCache.lastSelection = m.selection
	}

	// Invalidate content lines when content changes — they'll be rebuilt
	// lazily on demand in extractSelectedText (only needed for selection).
	if contentChanged {
		m.viewCache.lastContentStr = contentStr
		m.contentLines = nil
	}

	// Post-process: apply selection highlight
	viewOutput := m.viewCache.lastViewportView
	if m.selection.Active {
		viewOutput = m.applySelectionHighlight(viewOutput)
	}

	// Render viewport (scrollable area)
	b.WriteString(viewOutput)
	renderedLines += lipgloss.Height(m.viewCache.lastViewportView)
	b.WriteString("\n")
	renderedLines++

	m.applyFooterLayout(renderedLines, footer)
	b.WriteString(footer.view)

	return m.overlayAltScreenPanels(b.String(), footer)
}

func (m *Model) overlayAltScreenPanels(base string, footer footerLayout) string {
	// In alt-screen mode Bubble Tea v2 clips anything that extends beyond the
	// fixed terminal height, so popups must be composited into the existing frame
	// instead of being appended below it.
	if !m.completions.IsVisible() && !m.dialog.IsOpen() {
		return base
	}

	screenWidth := m.width
	if screenWidth <= 0 {
		screenWidth = lipgloss.Width(base)
	}
	if screenWidth <= 0 {
		screenWidth = 1
	}

	lines := strings.Split(base, "\n")
	targetHeight := m.height
	if targetHeight <= 0 {
		targetHeight = len(lines)
	}
	if targetHeight <= 0 {
		targetHeight = 1
	}

	blankLine := strings.Repeat(" ", screenWidth)
	for len(lines) < targetHeight {
		lines = append(lines, blankLine)
	}
	if len(lines) > targetHeight {
		lines = lines[:targetHeight]
	}

	bottomY := targetHeight - footer.height
	if bottomY < 0 {
		bottomY = 0
	}

	stackPanel := func(panel string) {
		if panel == "" {
			return
		}
		panelLines := strings.Split(panel, "\n")
		y := bottomY - len(panelLines)
		if y < 0 {
			y = 0
		}
		for i, panelLine := range panelLines {
			row := y + i
			if row < 0 || row >= len(lines) {
				continue
			}
			overlayWidth := lipgloss.Width(panelLine)
			if overlayWidth <= 0 {
				continue
			}
			overlay := ansi.Cut(panelLine, 0, screenWidth)
			if overlayWidth > screenWidth {
				overlayWidth = screenWidth
			}
			remainder := ansi.Cut(lines[row], overlayWidth, screenWidth)
			lines[row] = overlay + remainder
		}
		bottomY = y
	}

	// Preserve the existing visual order above the footer:
	// completions above dialog above footer.
	if m.dialog.IsOpen() {
		stackPanel(m.dialog.View())
	}
	if m.completions.IsVisible() {
		stackPanel(m.completions.View())
	}

	return strings.Join(lines, "\n")
}

type footerLayout struct {
	view            string
	height          int
	textareaOffsetY int
	textareaHeight  int
}

func (m *Model) wrapFooterLine(line string) string {
	if line == "" || m.width <= 0 {
		return line
	}
	return lipgloss.NewStyle().Width(m.width).MaxWidth(m.width).Render(line)
}

func (m *Model) buildFooterLayout() footerLayout {
	theme := m.styles.Theme()
	separator := lipgloss.NewStyle().Foreground(theme.Muted).Render(strings.Repeat("─", m.width))
	var rows []string
	rows = append(rows, separator)
	textareaOffsetY := 1

	appendMetaRow := func(row string) {
		if row == "" {
			return
		}
		row = m.wrapFooterLine(row)
		rows = append(rows, row)
		textareaOffsetY += lipgloss.Height(row)
	}

	if m.interruptNotice != "" {
		noticeStyle := lipgloss.NewStyle().Foreground(theme.Muted).Italic(true)
		appendMetaRow(noticeStyle.Render("  " + m.interruptNotice))
	}

	if m.pendingInterjection != "" {
		pendingStyle := lipgloss.NewStyle().Foreground(theme.Muted).Italic(true)
		pendingText := m.pendingInterjection
		label := "will incorporate"
		switch m.pendingInterruptUI {
		case "deciding":
			label = "deciding…"
		case "interject":
			label = "will incorporate"
		}
		// Truncate long messages before wrapping so very narrow widths remain stable.
		maxLen := m.width - 20 // account for prefix/suffix
		if maxLen > 0 && len(pendingText) > maxLen {
			pendingText = pendingText[:maxLen] + "…"
		}
		appendMetaRow(pendingStyle.Render("  ⏳ " + pendingText + " (" + label + ")"))
	}

	if len(m.files) > 0 {
		var fileNames []string
		for _, f := range m.files {
			fileNames = append(fileNames, f.Name)
		}
		filesInfo := lipgloss.NewStyle().Foreground(theme.Secondary).Render(
			fmt.Sprintf("[with: %s]", strings.Join(fileNames, ", ")))
		appendMetaRow(filesInfo)
	}

	if len(m.images) > 0 {
		muted := lipgloss.NewStyle().Foreground(theme.Muted)
		selected := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true).Underline(true)
		var chips []string
		for i := range m.images {
			label := fmt.Sprintf("[image %d]", i+1)
			if i == m.selectedImage {
				chips = append(chips, selected.Render(label))
			} else {
				chips = append(chips, muted.Render(label))
			}
		}
		appendMetaRow(strings.Join(chips, " "))
	}

	textareaView := m.textarea.View()
	rows = append(rows, textareaView)
	rows = append(rows, separator)
	rows = append(rows, m.renderStatusLine())

	view := strings.Join(rows, "\n")
	return footerLayout{
		view:            view,
		height:          lipgloss.Height(view),
		textareaOffsetY: textareaOffsetY,
		textareaHeight:  lipgloss.Height(textareaView),
	}
}

// viewAutoSend renders a minimal view for auto-send benchmarking mode.
// This skips expensive UI elements like textarea, separators, and status line
// to measure pure LLM response time without rendering overhead.
func (m *Model) viewAutoSend() string {
	if m.streaming {
		// Minimal status line during streaming
		elapsed := time.Since(m.streamStartTime)
		return fmt.Sprintf("%s%s:%s · mcp:off · %s  Responding %s",
			m.agentPrefix(), m.providerName, m.modelName, m.spinner.View(), formatChatElapsed(elapsed))
	}
	return ""
}

// agentPrefix returns "agentname · " when an agent is set, or "" otherwise.
func (m *Model) agentPrefix() string {
	if m.agentName != "" {
		return m.agentName + " · "
	}
	return ""
}

func (m *Model) renderMd(text string, width int) string {
	if text == "" {
		return ""
	}
	return m.renderMarkdown(text)
}

// maybeFlushToScrollback checks if there are segments to flush to scrollback,
// keeping View() small to avoid terminal scroll issues.
// In alt screen mode, we never flush to scrollback since View() renders everything.
func (m *Model) maybeFlushToScrollback() tea.Cmd {
	if m.altScreen || m.tracker == nil {
		return nil
	}

	result := m.tracker.FlushToScrollback(m.width, 0, maxViewLines, m.renderMd)
	if result.ToPrint != "" {
		return tea.Println(result.ToPrint)
	}
	return nil
}

// renderStreamingInline renders the streaming response for inline mode
func (m *Model) renderStreamingInline() string {
	var b strings.Builder

	// Cache rendered completed segments - only rebuild when tracker.Version changes
	// This avoids expensive re-rendering during wave animation
	var content string
	if m.tracker != nil && m.viewCache.cachedTrackerVersion == m.tracker.Version {
		content = m.viewCache.cachedCompletedContent
	} else if m.tracker != nil {
		// Render completed segments (segment-based tracking handles what's already flushed)
		// In alt screen mode, include images since we never flush to scrollback
		content = m.tracker.RenderUnflushed(m.width, m.renderMd, m.altScreen)
		m.viewCache.cachedCompletedContent = content
		m.viewCache.cachedTrackerVersion = m.tracker.Version
	}

	if content != "" {
		b.WriteString(content)
	}

	// Show the indicator with current phase, unless paused for external UI
	if !m.pausedForExternalUI {
		wavePos := 0
		var active []*ui.Segment
		if m.tracker != nil {
			wavePos = m.tracker.WavePos
			active = m.tracker.ActiveSegments()
		}

		hasContent := b.Len() > 0
		if hasContent {
			if len(active) > 0 && m.tracker != nil {
				completed := m.tracker.CompletedSegments()
				if len(completed) > 0 {
					b.WriteString(ui.SegmentSeparator(completed[len(completed)-1].Type, active[0].Type))
				}
			} else {
				b.WriteString("\n")
			}
		}

		indicator := ui.StreamingIndicator{
			Spinner:         m.spinner.View(),
			Phase:           m.phase,
			Elapsed:         time.Since(m.streamStartTime),
			Tokens:          m.currentTokens,
			ShowCancel:      true,
			HideProgress:    true, // progress shown in status line instead
			Segments:        active,
			WavePos:         wavePos,
			Width:           m.width,
			RenderMarkdown:  m.renderMd,
			HasFlushed:      !hasContent && m.tracker != nil && m.tracker.HasFlushed,
			LastFlushedType: m.tracker.LastFlushedType,
		}
		b.WriteString(indicator.Render(m.styles))
		b.WriteString("\n")

		// Retry status if present (shown as warning on separate line)
		if m.retryStatus != "" {
			b.WriteString(lipgloss.NewStyle().Foreground(m.styles.Theme().Warning).Render("⚠ " + m.retryStatus))
			b.WriteString("\n")
		}
	}

	return b.String()
}

// renderInputInline renders the input prompt for inline mode
func (m *Model) renderInputInline() string {
	return m.buildFooterLayout().view
}

// renderError renders the error message when m.err is set
func (m *Model) renderError() string {
	if m.err == nil {
		return ""
	}

	// User cancellation gets a softer message
	if errors.Is(m.err, context.Canceled) {
		return m.styles.Muted.Render("(cancelled)")
	}

	// API errors: red circle + red error text
	errMsg := m.err.Error()
	return ui.ErrorCircle() + " " + m.styles.Error.Render("Error: "+errMsg)
}

// updateTextareaHeight adjusts textarea height based on content lines including wrapping
func (m *Model) updateTextareaHeight() {
	content := m.textarea.Value()
	textareaWidth := m.textarea.Width()
	if textareaWidth <= 0 {
		textareaWidth = m.width
	}

	// Account for prompt width
	effectiveWidth := textareaWidth - lipgloss.Width("❯ ")
	if effectiveWidth <= 0 {
		effectiveWidth = 1
	}

	// Count visual lines (accounting for word wrap)
	visualLines := 0
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		lineLen := lipgloss.Width(line)
		if lineLen == 0 {
			visualLines++
		} else {
			visualLines += (lineLen + effectiveWidth - 1) / effectiveWidth
		}
	}

	if visualLines < 1 {
		visualLines = 1
	}

	// Limit height to about 1/3 of the screen or at least 5 lines
	maxHeight := m.height / 3
	if maxHeight < 5 {
		maxHeight = 5
	}
	if visualLines > maxHeight {
		visualLines = maxHeight
	}

	m.textarea.SetHeight(visualLines)
}

// setTextareaValue sets the textarea value and updates its height for proper wrapping
func (m *Model) setTextareaValue(s string) {
	m.textarea.SetValue(s)
	m.updateTextareaHeight()
}

// reflowTextarea wraps long lines in the textarea content so they're visible.
// Called after paste to insert hard newlines where the textarea would visually wrap.
func (m *Model) reflowTextarea() {
	content := m.textarea.Value()
	if content == "" {
		return
	}

	textareaWidth := m.textarea.Width()
	if textareaWidth <= 0 {
		textareaWidth = m.width
	}
	effectiveWidth := textareaWidth - lipgloss.Width("❯ ")
	if effectiveWidth < 20 {
		effectiveWidth = 20
	}

	wrapped := wordwrap.String(content, effectiveWidth)
	if wrapped != content {
		m.textarea.SetValue(wrapped)
	}
}

func formatChatElapsed(elapsed time.Duration) string {
	seconds := int(elapsed / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf("%ds", seconds)
}

// renderStatusLine renders a tiny status line showing model and options
func (m *Model) renderStatusLine() string {
	theme := m.styles.Theme()
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	successStyle := lipgloss.NewStyle().Foreground(theme.Success)

	const sep = " · "

	// Build fixed parts first (these are always shown as-is)
	var fixedParts []string

	// Agent name first if set.
	if m.agentName != "" {
		fixedParts = append(fixedParts, m.agentName)
	}

	// Model label (after agent, to keep agent identity prominent).
	model := shortenModelName(m.modelName)
	if model != "" {
		fixedParts = append(fixedParts, model)
	} else if m.providerName != "" {
		fixedParts = append(fixedParts, m.providerName)
	}

	if m.yolo {
		fixedParts = append(fixedParts, mutedStyle.Render("yolo"))
	}

	// Web search status
	if m.searchEnabled {
		fixedParts = append(fixedParts, successStyle.Render("web:on"))
	}

	// File count if any
	if len(m.files) > 0 {
		fixedParts = append(fixedParts, fmt.Sprintf("%d file(s)", len(m.files)))
	}
	if len(m.images) > 0 {
		fixedParts = append(fixedParts, fmt.Sprintf("%d image(s)", len(m.images)))
	}
	// Token usage counter (e.g., ~45K/136K) with optional cached segment
	usagePart := ""
	if m.engine != nil && m.engine.InputLimit() > 0 {
		contextTokens := m.engine.EstimateTokens(m.buildMessagesForContextEstimate())
		limit := m.engine.InputLimit()
		if contextTokens > 0 && limit > 0 {
			usagePart = fmt.Sprintf("~%s/%s",
				llm.FormatTokenCount(contextTokens), llm.FormatTokenCount(limit))
		}
	}

	cachedInputTokens := 0
	if m.stats != nil && m.stats.CachedInputTokens > 0 {
		cachedInputTokens = m.stats.CachedInputTokens
	}
	if cachedInputTokens > 0 {
		cachedLabel := llm.FormatTokenCount(cachedInputTokens)
		if cachedLabel != "" {
			cachePart := fmt.Sprintf("%s cached", cachedLabel)
			shortCachePart := fmt.Sprintf("cache:%s", cachedLabel)
			useShortCacheLabel := m.width > 0 && m.width < 40
			if usagePart != "" {
				if useShortCacheLabel {
					usagePart = fmt.Sprintf("%s (%s)", usagePart, shortCachePart)
				} else {
					usagePart = fmt.Sprintf("%s (%s)", usagePart, cachePart)
				}
			} else {
				if useShortCacheLabel {
					usagePart = shortCachePart
				} else {
					usagePart = cachePart
				}
			}
		}
	}
	if usagePart != "" {
		fixedParts = append(fixedParts, usagePart)
	}

	// During streaming, add progress info
	var streamingPart string
	if m.streaming {
		elapsed := time.Since(m.streamStartTime)
		progressParts := []string{m.spinner.View() + " " + m.phase}
		if m.currentTokens > 0 {
			progressParts = append(progressParts, fmt.Sprintf("%d tok", m.currentTokens))
		}
		progressParts = append(progressParts, formatChatElapsed(elapsed))
		streamingPart = strings.Join(progressParts, " ")
	}

	// Calculate used width from fixed parts
	// Use lipgloss.Width which properly strips ANSI escape sequences
	fixedWidth := 0
	for i, p := range fixedParts {
		fixedWidth += lipgloss.Width(p)
		if i > 0 {
			fixedWidth += len(sep)
		}
	}
	if streamingPart != "" {
		fixedWidth += len(sep) + lipgloss.Width(streamingPart)
	}

	// Calculate available width for tools and mcp
	availableWidth := m.width - fixedWidth

	// Build tools string - show count when 4+ tools, full names only for small sets
	var toolsPart string
	if len(m.localTools) > 0 {
		if len(m.localTools) == len(tools.AllToolNames()) {
			toolsPart = successStyle.Render("tools:all")
		} else if len(m.localTools) >= 4 {
			toolsPart = successStyle.Render(fmt.Sprintf("tools:%d", len(m.localTools)))
		} else {
			fullTools := "tools:" + strings.Join(m.localTools, ",")
			shortTools := fmt.Sprintf("tools:%d", len(m.localTools))
			// Account for separator
			needed := len(sep) + len(fullTools)
			if needed <= availableWidth {
				toolsPart = successStyle.Render(fullTools)
			} else {
				toolsPart = successStyle.Render(shortTools)
			}
		}
	}

	// Build mcp string - use full names if they fit, otherwise abbreviate
	var mcpPart string
	if m.mcpManager != nil {
		available := m.mcpManager.AvailableServers()
		if len(available) > 0 {
			enabled := m.mcpManager.EnabledServers()
			if len(enabled) > 0 {
				fullMcp := "mcp:" + strings.Join(enabled, ",")
				shortMcp := fmt.Sprintf("mcp:%d", len(enabled))
				// Account for separator and tools part
				usedByTools := 0
				if toolsPart != "" {
					usedByTools = len(sep) + lipgloss.Width(toolsPart)
				}
				needed := len(sep) + len(fullMcp)
				if needed <= availableWidth-usedByTools {
					mcpPart = successStyle.Render(fullMcp)
				} else {
					mcpPart = successStyle.Render(shortMcp)
				}
			} else {
				mcpPart = mutedStyle.Render("mcp:off")
			}
		}
	}

	// Combine all parts
	var parts []string
	parts = append(parts, fixedParts...)
	if toolsPart != "" {
		parts = append(parts, toolsPart)
	}
	if mcpPart != "" {
		parts = append(parts, mcpPart)
	}
	if streamingPart != "" {
		parts = append(parts, streamingPart)
	}

	// Selection hint
	if m.selection.Active {
		start, end := m.selection.Normalized()
		lines := end.Line - start.Line + 1
		if start.Line == end.Line && start.Col == end.Col {
			lines = 0
		}
		if lines > 0 {
			parts = append(parts, fmt.Sprintf("%d lines · ctrl+y:copy", lines))
		}
	}
	if m.copyStatus != "" {
		parts = append(parts, m.copyStatus)
	}

	return m.wrapFooterLine(mutedStyle.Render(strings.Join(parts, sep)))
}

// mcpFindServerMatch finds the best matching server name for tab completion
func (m *Model) mcpFindServerMatch(partial string) string {
	if m.mcpManager == nil {
		return ""
	}
	available := m.mcpManager.AvailableServers()
	partialLower := strings.ToLower(partial)

	// Try prefix match first
	for _, s := range available {
		if strings.HasPrefix(strings.ToLower(s), partialLower) {
			return s
		}
	}
	// Try contains match
	for _, s := range available {
		if strings.Contains(strings.ToLower(s), partialLower) {
			return s
		}
	}
	return ""
}

// updateCompletions updates the completions popup based on current input.
// Handles both static command completions and dynamic argument completions.
func (m *Model) updateCompletions() {
	value := m.textarea.Value()
	query := strings.TrimPrefix(value, "/")

	// Check for MCP server argument completions
	// /mcp start <server>, /mcp stop <server>, /mcp add <server>
	lowerQuery := strings.ToLower(query)

	// Check for "/mcp start ", "/mcp stop ", "/mcp restart " - show configured servers
	if strings.HasPrefix(lowerQuery, "mcp start ") ||
		strings.HasPrefix(lowerQuery, "mcp stop ") ||
		strings.HasPrefix(lowerQuery, "mcp restart ") {
		if m.mcpManager != nil {
			// Extract the partial server name after the subcommand
			parts := strings.SplitN(query, " ", 3)
			partial := ""
			if len(parts) >= 3 {
				partial = strings.ToLower(parts[2])
			}

			// Get configured servers
			servers := m.mcpManager.AvailableServers()
			var items []Command
			for _, s := range servers {
				if partial == "" || strings.Contains(strings.ToLower(s), partial) {
					status, _ := m.mcpManager.ServerStatus(s)
					desc := "stopped"
					if status == "ready" {
						desc = "running"
					} else if status == "starting" {
						desc = "starting..."
					}
					items = append(items, Command{
						Name:        parts[0] + " " + parts[1] + " " + s,
						Description: desc,
					})
				}
			}
			m.completions.SetItems(items)
			return
		}
	}

	// Check for "/mcp add " - show bundled servers not yet configured
	if strings.HasPrefix(lowerQuery, "mcp add ") {
		bundled := mcp.GetBundledServers()

		// Get already configured servers
		configured := make(map[string]bool)
		if m.mcpManager != nil {
			for _, s := range m.mcpManager.AvailableServers() {
				configured[strings.ToLower(s)] = true
			}
		}

		// Extract partial name
		parts := strings.SplitN(query, " ", 3)
		partial := ""
		if len(parts) >= 3 {
			partial = strings.ToLower(parts[2])
		}

		var items []Command
		for _, s := range bundled {
			if configured[strings.ToLower(s.Name)] {
				continue // Skip already configured
			}
			if partial == "" || strings.Contains(strings.ToLower(s.Name), partial) {
				items = append(items, Command{
					Name:        "mcp add " + s.Name,
					Description: s.Description,
				})
			}
			if len(items) >= 15 { // Limit to avoid huge list
				break
			}
		}
		m.completions.SetItems(items)
		return
	}

	// Check for "/model " or "/m " - show provider:model completions
	if strings.HasPrefix(lowerQuery, "model ") || strings.HasPrefix(lowerQuery, "m ") {
		parts := strings.SplitN(query, " ", 2)
		partial := ""
		if len(parts) == 2 {
			partial = parts[1]
		}
		m.completions.SetItems(providerModelCompletionItems(parts[0]+" ", partial, m.config))
		return
	}

	// Check for "/handover " or "/ho " - show available agents, then provider:model overrides
	if strings.HasPrefix(lowerQuery, "handover ") || strings.HasPrefix(lowerQuery, "ho ") {
		parts := strings.SplitN(query, " ", 3)
		if len(parts) == 3 && strings.HasPrefix(parts[1], "@") {
			m.completions.SetItems(providerModelCompletionItems(parts[0]+" "+parts[1]+" ", parts[2], m.config))
			return
		}
		if m.agentLister != nil {
			partial := ""
			if len(parts) >= 2 {
				partial = strings.ToLower(strings.TrimPrefix(parts[1], "@"))
			}

			names, err := m.agentLister(m.config)
			if err == nil {
				var items []Command
				for _, name := range names {
					if partial == "" || strings.Contains(strings.ToLower(name), partial) {
						items = append(items, Command{
							Name:        parts[0] + " @" + name,
							Description: "agent",
						})
					}
				}
				m.completions.SetItems(items)
				return
			}
		}
	}

	// Default: use standard command filtering
	m.completions.SetQuery(query)
}

// shortenModelName removes date suffixes from model names (e.g., "claude-sonnet-4-20250514" -> "claude-sonnet-4")
func shortenModelName(name string) string {
	// Remove date suffix pattern like -20250514 or -20241022
	if len(name) > 9 {
		suffix := name[len(name)-9:]
		if suffix[0] == '-' && isAllDigits(suffix[1:]) {
			return name[:len(name)-9]
		}
	}
	return name
}

// isAllDigits checks if a string contains only digits
func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// getTerminalTitle returns the appropriate terminal title based on state
func (m *Model) getTerminalTitle() string {
	if m.streaming {
		elapsed := time.Since(m.streamStartTime)
		return fmt.Sprintf("term-llm · %s... (%.0fs)", m.phase, elapsed.Seconds())
	}

	msgCount := len(m.messages)
	if msgCount == 0 {
		return "term-llm chat"
	}

	return fmt.Sprintf("term-llm · %d messages · %s", msgCount, m.modelName)
}

func (m *Model) renderHistory() string {
	if len(m.messages) == 0 {
		return render.RenderEmptyHistory(m.styles.Theme())
	}

	var mode render.RenderMode
	if m.altScreen {
		mode = render.RenderModeAltScreen
	} else {
		mode = render.RenderModeInline
	}

	// Prepare messages to render
	// In alt-screen mode, viewport handles scrolling via YOffset, so render all messages
	// In inline mode, use message-based scrollOffset to slice visible messages
	messages := m.messages
	scrollOffset := 0
	if !m.altScreen && m.scrollOffset > 0 {
		// Inline mode: pre-slice messages based on scroll offset
		endIdx := len(messages) - m.scrollOffset
		if endIdx < 1 {
			endIdx = 1
		}
		messages = messages[:endIdx]
		scrollOffset = m.scrollOffset
	}

	// In alt screen mode, skip all messages from the last turn if completedStream is showing it.
	// completedStream contains everything from the tracker (all turns since the last user message).
	if m.altScreen && m.viewCache.completedStream != "" && len(messages) > 0 {
		i := len(messages) - 1
		// Skip all assistant and tool messages at the end of the list
		for i >= 0 && (messages[i].Role == llm.RoleAssistant || messages[i].Role == llm.RoleTool) {
			i--
		}
		// Include up to the last user message
		messages = messages[:i+1]
	}

	state := render.RenderState{
		Messages: messages,
		Viewport: render.ViewportState{
			Height:       m.viewportRows,
			ScrollOffset: scrollOffset,
			AtBottom:     scrollOffset == 0,
		},
		Mode:   mode,
		Width:  m.width,
		Height: m.height,
	}

	var b strings.Builder

	// Show scroll indicator if not at bottom
	if m.scrollOffset > 0 {
		b.WriteString(render.RenderScrollIndicator(m.scrollOffset, m.styles.Theme()))
	}

	// Render messages using virtualized renderer
	b.WriteString(m.chatRenderer.Render(state))

	return b.String()
}

func (m *Model) renderMarkdown(content string) string {
	if content == "" {
		return ""
	}

	// In text mode, skip markdown rendering but still apply word wrapping.
	if m.textMode {
		targetWidth := m.width - 2
		if targetWidth < 20 {
			targetWidth = 20
		}
		return wordwrap.String(content, targetWidth)
	}

	return ui.RenderMarkdownWithOptions(content, m.width, ui.MarkdownRenderOptions{
		WrapOffset:        2,
		NormalizeTabs:     true,
		NormalizeNewlines: false,
	})
}

func (m *Model) shouldThrottleSetContent(now time.Time) bool {
	if !m.streaming {
		return false
	}
	if m.streamRenderMinInterval <= 0 {
		return false
	}
	if m.approvalModel != nil || m.askUserModel != nil {
		return false
	}
	if m.scrollToBottom {
		return false
	}
	if m.viewCache.lastSetContentAt.IsZero() {
		return false
	}
	return now.Sub(m.viewCache.lastSetContentAt) < m.streamRenderMinInterval
}

// GetBundledServers returns bundled MCP servers (wrapper for mcp package)
func GetBundledServers() []mcp.BundledServer {
	return mcp.GetBundledServers()
}
