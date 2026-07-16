package chat

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	render "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// maxViewLines is the maximum number of lines to keep in View().
// Content beyond this is printed to scrollback to prevent scroll issues.
const maxViewLines = 8

// View renders the model
func (m *Model) View() tea.View {
	// Return a mode-free frame before ExecProcess releases the renderer. This
	// prevents a queued render from re-enabling alt-screen, mouse, or keyboard
	// protocols while an interactive child owns the terminal. Post-frame output
	// is suppressed separately as the hard guard against asynchronous writes.
	if m.externalProcessActive {
		return tea.NewView("")
	}
	if m.quitting {
		return m.newView("")
	}
	m.textareaBoundsValid = false

	// Inspector mode uses alternate screen
	if m.inspectorMode && m.inspectorModel != nil {
		return m.inspectorModel.View()
	}

	// Worktree browser mode uses its dedicated full-screen view.
	if m.worktreeBrowserMode && m.worktreeBrowserModel != nil {
		return m.worktreeBrowserModel.View()
	}

	// Resume browser mode uses the dedicated sessions browser view
	if m.resumeBrowserMode && m.resumeBrowserModel != nil {
		return m.resumeBrowserModel.View()
	}

	if m.streaming && m.streamPerf != nil {
		m.streamPerf.RecordFrameAt(time.Now())
	}

	// Alt screen mode: use viewport for scrollable content
	if m.altScreen {
		content := m.viewAltScreen()
		if m.sideQuestion.Visible {
			content = m.renderSideQuestionOverlay(content)
		}
		return m.newView(content)
	}

	// Auto-send mode: minimal rendering for benchmarking (skip expensive UI)
	if m.autoSendQueue != nil {
		return m.newView(m.viewAutoSend())
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

	content := b.String()
	if m.sideQuestion.Visible {
		content += "\n" + m.renderSideQuestionPanel()
	}
	return m.newView(content)
}

// newView wraps content in a tea.View with the model's declarative flags.
func (m *Model) newView(content string) tea.View {
	v := tea.NewView(content)
	if m.altScreen {
		v.AltScreen = true
	}
	if m.autoSendQueue == nil && m.mouseMode {
		v.MouseMode = tea.MouseModeCellMotion
	}
	if cur := m.imageSafeCursor(); cur != nil {
		v.Cursor = cur
		return v
	}
	if cur := m.composerCursor(); cur != nil {
		v.Cursor = cur
	}
	return v
}

func (m *Model) composerCursor() *tea.Cursor {
	if m == nil || !m.textareaBoundsValid || !m.textarea.Focused() {
		return nil
	}
	if m.autoSendQueue != nil || m.quitting || m.sideQuestion.Visible {
		return nil
	}
	if m.dialog != nil && m.dialog.IsOpen() {
		return nil
	}
	if m.approvalModel != nil || m.askUserModel != nil || m.handoverPreview != nil {
		return nil
	}
	cur := m.textarea.Cursor()
	if cur == nil {
		return nil
	}
	cur.Position.X += m.textareaLeftX
	cur.Position.Y += m.textareaTopY
	if m.width > 0 {
		cur.Position.X = max(0, min(cur.Position.X, m.width-1))
	}
	if m.height > 0 {
		cur.Position.Y = max(0, min(cur.Position.Y, m.height-1))
	}
	return cur
}

func (m *Model) needsImageSafeCursor() bool {
	return m != nil && m.altScreen && (len(m.viewportImageArtifacts) > 0 || len(m.pendingImageUploads) > 0 || len(m.uploadedImageKeys) > 0 || len(m.placedImageKeys) > 0)
}

func (m *Model) imageSafeCursor() *tea.Cursor {
	if !m.needsImageSafeCursor() {
		return nil
	}
	cur := tea.NewCursor(0, max(0, m.height-1))
	cur.Shape = tea.CursorBar
	cur.Blink = false
	return cur
}

// viewAltScreen renders the full-screen alt screen view with scrollable viewport
func (m *Model) viewAltScreen() string {
	var b strings.Builder
	renderedLines := 0
	footer := m.buildFooterLayout()
	m.syncAltScreenViewportHeight(footer.height)
	m.resetViewportHorizontalOffset()

	// Build scrollable content with caching to avoid re-rendering unchanged content

	// Check if history cache is valid.
	// Skip expensive signature computation when the cache is already valid and
	// dimensions/scroll haven't changed — this eliminates O(total_content_bytes)
	// hashing on every frame during streaming.
	historyValid := m.viewCache.historyValid &&
		m.viewCache.historyWidth == m.width &&
		m.viewCache.historyScrollOffset == m.scrollOffset
	if !historyValid {
		historySig := m.chatRenderer.CachedHistorySignature(m.messages)
		if m.viewCache.historyValid && m.viewCache.historySignature == historySig && m.viewCache.historyWidth == m.width {
			// Content unchanged despite invalidation (e.g. scroll offset change) — restore validity.
			m.viewCache.historyScrollOffset = m.scrollOffset
		} else {
			m.resetAltScreenStreamingAppendCache()
			m.viewCache.historyContent = m.renderHistory()
			m.viewCache.historyLines = splitViewportContentLines(m.viewCache.historyContent)
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
	var contentLines []string
	var streamingContent string
	usedIncrementalAppend := false
	waveOnlyChanged := false
	if m.streaming {
		if m.tracker != nil {
			trackerVersion := m.tracker.Version
			if m.streamPerf != nil {
				m.streamPerf.RecordTrackerVersion(trackerVersion)
			}
			wavePos := m.tracker.WavePos
			trackerChanged := trackerVersion != m.viewCache.lastTrackerVersion
			waveChanged := wavePos != m.viewCache.lastWavePos && m.tracker.HasPending()
			if trackerChanged {
				m.viewCache.lastTrackerVersion = trackerVersion
				m.viewCache.lastWavePos = wavePos
				m.bumpContentVersion()
			} else if waveChanged {
				m.viewCache.lastWavePos = wavePos
				waveOnlyChanged = true
			}
		}
	}

	// Only call SetContent if content actually changed (expensive operation)
	// Use version comparison instead of O(n) string comparison
	contentChanged := m.viewCache.contentVersion != m.viewCache.lastRenderedVersion
	contentDirty := contentChanged

	// Force update if embedded UI is active (since it's interactive and doesn't affect tracker version)
	if m.approvalModel != nil || m.askUserModel != nil || m.handoverPreview != nil {
		contentChanged = true
		contentDirty = true
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
			streamingContent = m.renderStreamingInline()
			if m.approvalModel == nil && m.askUserModel == nil && m.handoverPreview == nil {
				contentLines, usedIncrementalAppend = m.tryAppendAltScreenStreamingContent(streamingContent)
			}
			if !usedIncrementalAppend {
				contentStr = m.viewCache.historyContent + streamingContent
				if m.approvalModel != nil {
					contentStr += "\n" + m.approvalModel.View().Content
				} else if m.askUserModel != nil {
					contentStr += "\n" + m.askUserModel.View().Content
				} else if m.handoverPreview != nil {
					contentStr += m.handoverPreview.View()
				}
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

		if usedIncrementalAppend {
			joined := strings.Join(contentLines, "\n")
			var blocks []viewportImageBlock
			joined, blocks = m.extractViewportImageBlocks(joined)
			m.viewportImageBlocks = blocks
			contentLines = splitViewportContentLines(joined)
		} else {
			var blocks []viewportImageBlock
			contentStr, blocks = m.extractViewportImageBlocks(contentStr)
			m.viewportImageBlocks = blocks
		}

		// Check if user is at bottom BEFORE setting content (which changes maxYOffset)
		wasAtBottom := m.viewport.AtBottom()
		firstRender := m.viewCache.lastViewportView == ""
		setContentStart := time.Now()
		if usedIncrementalAppend {
			m.viewport.SetContentLines(contentLines)
		} else {
			m.viewport.SetContent(contentStr)
		}
		setContentEnd := time.Now()
		if m.streamPerf != nil {
			m.streamPerf.RecordDuration(durationMetricSetContent, setContentEnd.Sub(setContentStart))
		}
		m.viewCache.lastSetContentAt = setContentEnd
		m.captureReasoningClickSnapshot()
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
	xOffsetChanged := m.viewport.XOffset() != m.viewCache.lastXOffset
	sizeChanged := m.viewport.Width() != m.viewCache.lastVPWidth || m.viewport.Height() != m.viewCache.lastVPHeight

	// Force re-render when selection changes
	selectionChanged := m.selection != m.viewCache.lastSelection
	needViewRender := contentChanged || waveOnlyChanged || yOffsetChanged || xOffsetChanged || sizeChanged || selectionChanged || m.viewCache.lastViewportView == ""
	if needViewRender {
		viewStart := time.Now()
		if waveOnlyChanged && !contentDirty {
			streamingContent = m.renderStreamingInline()
			contentLines = m.renderAltScreenStreamingContentLines(streamingContent)
			m.viewCache.lastViewportView = m.renderAltScreenViewportLines(contentLines)
			m.viewCache.lastContentStr = ""
			m.contentLines = contentLines
			m.viewCache.lastContentHistoryPlusStream = true
			m.viewCache.lastStreamingContent = streamingContent
		} else {
			if len(m.viewportImageBlocks) > 0 {
				lines := contentLines
				if len(lines) == 0 {
					if contentStr != "" {
						lines = splitViewportContentLines(contentStr)
					} else if len(m.contentLines) > 0 {
						lines = m.contentLines
					} else {
						lines = splitViewportContentLines(m.viewCache.lastContentStr)
					}
				}
				m.viewCache.lastViewportView = m.renderAltScreenViewportLinesWithImages(lines)
			} else {
				m.viewCache.lastViewportView = m.viewport.View()
			}
		}
		if m.streamPerf != nil {
			m.streamPerf.RecordDuration(durationMetricViewportView, time.Since(viewStart))
		}
		m.viewCache.lastYOffset = m.viewport.YOffset()
		m.viewCache.lastXOffset = m.viewport.XOffset()
		m.viewCache.lastVPWidth = m.viewport.Width()
		m.viewCache.lastVPHeight = m.viewport.Height()
		m.viewCache.lastSelection = m.selection
	}

	// Invalidate content lines when content changes — they'll be rebuilt
	// lazily on demand in extractSelectedText (only needed for selection).
	if contentChanged {
		if usedIncrementalAppend {
			m.viewCache.lastContentStr = ""
			m.contentLines = contentLines
			m.viewCache.lastContentHistoryPlusStream = true
		} else {
			m.viewCache.lastContentStr = contentStr
			m.contentLines = nil
			m.viewCache.lastContentHistoryPlusStream = m.streaming && m.approvalModel == nil && m.askUserModel == nil && m.handoverPreview == nil
		}
		if m.streaming {
			m.viewCache.lastStreamingContent = streamingContent
		} else {
			m.viewCache.lastStreamingContent = ""
		}
	}

	// Post-process: apply selection highlight
	viewOutput := m.viewCache.lastViewportView
	if m.selection.Active {
		viewOutput = m.applySelectionHighlight(viewOutput)
	}

	// Render viewport (scrollable area)
	b.WriteString(viewOutput)
	renderedLines += lipgloss.Height(m.viewCache.lastViewportView)
	footerStartY := renderedLines
	b.WriteString("\n")
	renderedLines++

	m.applyFooterLayout(footerStartY, footer)
	b.WriteString(footer.view)

	frame := m.overlayAltScreenPanels(b.String(), footer)
	if footerDebugEnabled() {
		if frameH := lipgloss.Height(frame); frameH != m.height {
			footerDebugf("frame_height_mismatch frame=%d term=%d vp=%d footer=%d wave_only=%v streaming=%v images=%d",
				frameH, m.height, m.viewport.Height(), footer.height, waveOnlyChanged, m.streaming, len(m.viewportImageBlocks))
		}
	}
	return frame
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

	placePanel := func(panel string, x, y int) {
		if panel == "" {
			return
		}
		panelLines := strings.Split(panel, "\n")
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
			if x < 0 {
				x = 0
			}
			if x >= screenWidth {
				continue
			}
			overlay := ansi.Cut(panelLine, 0, screenWidth-x)
			left := ansi.Cut(lines[row], 0, x)
			rightStart := x + min(overlayWidth, screenWidth-x)
			remainder := ansi.Cut(lines[row], rightStart, screenWidth)
			lines[row] = left + overlay + remainder
		}
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
		panel := m.dialog.View()
		if m.dialog.Type() == DialogContent {
			panelLines := strings.Split(panel, "\n")
			x := (screenWidth - lipgloss.Width(panel)) / 2
			y := (targetHeight - len(panelLines)) / 2
			placePanel(panel, x, y)
		} else {
			stackPanel(panel)
		}
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

	if queuedEffort, applied, ok := m.pendingStreamEffortStatus(); ok && !applied {
		pendingStyle := lipgloss.NewStyle().Foreground(theme.Muted).Italic(true)
		appendMetaRow(pendingStyle.Render("  ↻ reasoning effort " + queuedEffort + " queued (applies at next model turn)"))
	}

	if len(m.pendingInterjections) > 0 {
		pendingStyle := lipgloss.NewStyle().Foreground(theme.Muted).Italic(true)
		selectedStyle := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
		for i, pending := range m.pendingInterjections {
			pendingText := pending.Text
			label := "will incorporate"
			switch pending.UIState {
			case "deciding":
				label = "deciding…"
			case "interject":
				label = "will incorporate"
			}
			// Truncate long messages before wrapping so very narrow widths remain stable.
			maxLen := m.width - 26 // account for prefix/suffix
			if maxLen > 0 && len(pendingText) > maxLen {
				pendingText = pendingText[:maxLen] + "…"
			}
			prefix := "  ⏳ "
			if i == m.selectedInterjection {
				prefix = "  ▸ "
			}
			line := prefix + pendingText + " (" + label + ")"
			if i == m.selectedInterjection {
				line += "  [del cancels]"
				appendMetaRow(selectedStyle.Render(line))
			} else {
				appendMetaRow(pendingStyle.Render(line))
			}
		}
	} else if m.pendingInterjection != "" {
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
	statusLine := m.renderStatusLine()
	if footerDebugEnabled() && lipgloss.Width(statusLine) == 0 {
		footerDebugf("status_line_blank footer_message=%q width=%d streaming=%v phase=%q",
			m.footerMessage, m.width, m.streaming, m.phase)
	}
	rows = append(rows, statusLine)

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
			m.agentPrefix(), m.providerName, m.displayModelName(), m.spinner.View(), formatChatElapsed(elapsed))
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

func (m *Model) displayModelName() string {
	if m == nil {
		return ""
	}
	provider := strings.TrimSpace(m.providerKey)
	model := strings.TrimSpace(m.modelName)
	if m.streaming && m.pendingStreamModelSwitch != nil && m.pendingStreamModelSwitch.applied {
		pendingProvider := strings.TrimSpace(m.pendingStreamModelSwitch.provider)
		if pendingProvider == "" || pendingProvider == strings.TrimSpace(m.providerKey) || pendingProvider == strings.TrimSpace(m.providerName) {
			if pendingModel := strings.TrimSpace(m.pendingStreamModelSwitch.model); pendingModel != "" {
				model = pendingModel
				if pendingProvider != "" {
					provider = pendingProvider
				}
			}
		}
	}
	return m.displayNameForProviderModel(provider, model)
}

func (m *Model) displayNameForProviderModel(provider, model string) string {
	model = strings.TrimSpace(model)
	if m == nil || m.config == nil {
		return model
	}
	providers := []string{provider, m.providerKey, m.providerName}
	seen := make(map[string]bool, len(providers))
	for _, candidate := range providers {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		if display := config.DisplayModelForProviderModel(m.config, candidate, model); display != model {
			return display
		}
	}
	return model
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
		// In alt screen mode, include images since we never flush to scrollback.
		// The alt-screen image renderer keeps raw upload/control bytes out of the viewport.
		content = m.tracker.RenderUnflushedWithImageRenderer(m.width, m.renderMd, m.altScreen, m.imageArtifactRenderer())
		m.viewCache.cachedCompletedContent = content
		m.viewCache.cachedTrackerVersion = m.tracker.Version
	}

	if content != "" {
		b.WriteString(content)
	}

	if activeReasoning := m.renderCurrentReasoningBlock(); activeReasoning != "" {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n\n") {
			if strings.HasSuffix(b.String(), "\n") {
				b.WriteString("\n")
			} else {
				b.WriteString("\n\n")
			}
		} else if b.Len() == 0 && m.tracker != nil && m.tracker.HasFlushed {
			b.WriteString(m.tracker.FlushLeadingSeparator(ui.SegmentReasoning))
		}
		b.WriteString(strings.TrimLeft(activeReasoning, "\n"))
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
			ToolsExpanded:   m.toolsExpanded,
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
	return ui.FormatElapsedDuration(elapsed)
}

type statusSegment struct {
	text      string
	priority  int
	essential bool
	isUsage   bool // marks the context-usage segment so candidates can swap variants
	width     int  // cached display width of text; 0 means not yet measured
}

func (m *Model) statusLineGoalPart() (string, session.GoalStatus) {
	if m == nil || m.sess == nil || m.sess.Goal == nil || !m.sess.Goal.Exists() {
		return "", ""
	}
	goal := m.sess.Goal
	text := "🎯 " + string(goal.Status)
	if goal.TokenBudget > 0 {
		text += fmt.Sprintf(" · %d/%d tok", goal.TokensUsed, goal.TokenBudget)
	}
	return text, goal.Status
}

// renderStatusLine renders a tiny status line showing model and options
func (m *Model) renderStatusLine() string {
	theme := m.styles.Theme()
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	successStyle := lipgloss.NewStyle().Foreground(theme.Success)
	errorStyle := lipgloss.NewStyle().Foreground(theme.Error)
	warningStyle := lipgloss.NewStyle().Foreground(theme.Warning)

	if m.footerMessage != "" {
		style := mutedStyle
		switch m.footerMessageTone {
		case "muted":
			style = mutedStyle
		case "success":
			style = successStyle
		case "warning":
			style = warningStyle
		case "error":
			style = errorStyle
		default:
			lower := strings.ToLower(strings.TrimSpace(m.footerMessage))
			if strings.HasPrefix(lower, "failed") ||
				strings.HasPrefix(lower, "cannot") ||
				strings.HasPrefix(lower, "invalid") ||
				strings.HasPrefix(lower, "unknown") ||
				strings.HasPrefix(lower, "no ") ||
				strings.HasPrefix(lower, "not enough") ||
				strings.HasPrefix(lower, "file access denied") {
				style = errorStyle
			}
		}
		return m.wrapFooterLine(style.Render(m.footerMessage))
	}

	width := m.width
	if width <= 0 {
		width = 80
	}

	const sepText = " · "
	sep := mutedStyle.Render(sepText)
	sepWidth := lipgloss.Width(sep)
	seg := func(text string, priority int, essential bool) statusSegment {
		return statusSegment{text: text, width: lipgloss.Width(text), priority: priority, essential: essential}
	}

	usageLong, usageShort := m.statusLineUsageParts()

	baseSegments := make([]statusSegment, 0, 10)
	if m.agentName != "" {
		baseSegments = append(baseSegments, seg(mutedStyle.Render(m.agentName), 0, true))
	}
	model := shortenModelName(m.displayModelName())
	if model == "" && m.providerName != "" {
		model = m.providerName
	}
	if model != "" {
		baseSegments = append(baseSegments, seg(mutedStyle.Render(model), 0, true))
	}
	if m.sess != nil && strings.EqualFold(m.sess.ReasoningMode, "pro") {
		baseSegments = append(baseSegments, seg(successStyle.Render("pro"), 25, false))
	}
	if worktreeLabel := m.statusLineWorktreePart(); worktreeLabel != "" {
		baseSegments = append(baseSegments, seg(successStyle.Render(worktreeLabel), 0, true))
	}
	switch m.currentApprovalMode() {
	case tools.ModeAuto:
		baseSegments = append(baseSegments, seg(successStyle.Render("auto"), 40, false))
	case tools.ModeYolo:
		baseSegments = append(baseSegments, seg(mutedStyle.Render("yolo"), 40, false))
	}
	if m.searchEnabled {
		baseSegments = append(baseSegments, seg(successStyle.Render("web"), 30, false))
	}
	if m.fastMode {
		baseSegments = append(baseSegments, seg(successStyle.Render("fast"), 30, false))
	}
	if goalText, goalStatus := m.statusLineGoalPart(); goalText != "" {
		style := mutedStyle
		switch goalStatus {
		case session.GoalStatusActive:
			style = successStyle
		case session.GoalStatusPaused, session.GoalStatusBudgetLimited, session.GoalStatusBlocked:
			style = warningStyle
		case session.GoalStatusComplete:
			style = mutedStyle
		}
		baseSegments = append(baseSegments, seg(style.Render(goalText), 35, false))
	}
	if len(m.files) > 0 {
		baseSegments = append(baseSegments, seg(mutedStyle.Render(fmt.Sprintf("%d file(s)", len(m.files))), 55, false))
	}
	if len(m.images) > 0 {
		baseSegments = append(baseSegments, seg(mutedStyle.Render(fmt.Sprintf("%d image(s)", len(m.images))), 55, false))
	}
	if usageLong != "" {
		usageSeg := seg(mutedStyle.Render(usageLong), 50, true)
		usageSeg.isUsage = true
		baseSegments = append(baseSegments, usageSeg)
	}
	baseVariants := statusSegmentVariants(baseSegments)

	toolsFull, toolsShort := m.statusLineToolsParts(successStyle)
	mcpFull, mcpShort := m.statusLineMCPParts(successStyle, mutedStyle)

	rightVariants := m.statusLineStreamingVariants(mutedStyle)
	if len(rightVariants) == 0 {
		rightVariants = []string{""}
	}

	var candidates [][]statusSegment
	// Per-option rendered text/width/priority, computed once instead of per candidate.
	renderedUsage := map[string]statusSegment{}
	optionWidths := map[string]int{}
	mcpPriorities := map[string]int{}
	addCandidate := func(base []statusSegment, usage string, includeTools bool, toolsText string, includeMCP bool, mcpText string) {
		segments := make([]statusSegment, 0, len(base)+2)
		for _, segment := range base {
			if segment.isUsage {
				if usage == "" {
					continue
				}
				rendered := renderedUsage[usage]
				segment.text = rendered.text
				segment.width = rendered.width
			}
			segments = append(segments, segment)
		}
		if includeTools && toolsText != "" {
			segments = append(segments, statusSegment{text: toolsText, width: optionWidths[toolsText], priority: 20})
		}
		if includeMCP && mcpText != "" {
			segments = append(segments, statusSegment{text: mcpText, width: optionWidths[mcpText], priority: mcpPriorities[mcpText]})
		}
		candidates = append(candidates, segments)
	}

	toolOptions := []string{""}
	if toolsFull != "" {
		toolOptions = []string{toolsFull}
		if toolsShort != "" && ui.StripANSI(toolsShort) != ui.StripANSI(toolsFull) {
			toolOptions = append(toolOptions, toolsShort)
		}
		toolOptions = append(toolOptions, "")
	}
	mcpOptions := []string{""}
	if mcpFull != "" {
		mcpOptions = []string{mcpFull}
		if mcpShort != "" && ui.StripANSI(mcpShort) != ui.StripANSI(mcpFull) {
			mcpOptions = append(mcpOptions, mcpShort)
		}
		mcpOptions = append(mcpOptions, "")
	}
	usageOptions := []string{usageLong}
	if usageShort != "" && usageShort != usageLong {
		usageOptions = append(usageOptions, usageShort)
	}
	if usageLong != "" {
		usageOptions = append(usageOptions, "")
	}

	for _, usage := range usageOptions {
		if usage == "" {
			continue
		}
		rendered := mutedStyle.Render(usage)
		renderedUsage[usage] = statusSegment{text: rendered, width: lipgloss.Width(rendered)}
	}
	for _, text := range toolOptions {
		if text != "" {
			optionWidths[text] = lipgloss.Width(text)
		}
	}
	for _, text := range mcpOptions {
		if text == "" {
			continue
		}
		optionWidths[text] = lipgloss.Width(text)
		priority := 10
		if strings.Contains(ui.StripANSI(text), "mcp:off") {
			priority = 5
		}
		mcpPriorities[text] = priority
	}

	for _, usage := range usageOptions {
		if usage == "" {
			continue
		}
		for _, base := range baseVariants {
			for _, toolsText := range toolOptions {
				for _, mcpText := range mcpOptions {
					addCandidate(base, usage, toolsText != "", toolsText, mcpText != "", mcpText)
				}
			}
		}
	}
	if usageLong != "" {
		candidates = append(candidates, []statusSegment{seg(mutedStyle.Render(usageLong), 50, false)})
	}
	if usageShort != "" && usageShort != usageLong {
		candidates = append(candidates, []statusSegment{seg(mutedStyle.Render(usageShort), 50, false)})
	}
	if usageLong != "" {
		for _, base := range baseVariants {
			for _, toolsText := range toolOptions {
				for _, mcpText := range mcpOptions {
					addCandidate(base, "", toolsText != "", toolsText, mcpText != "", mcpText)
				}
			}
		}
	}
	if usageLong == "" {
		for _, base := range baseVariants {
			for _, toolsText := range toolOptions {
				for _, mcpText := range mcpOptions {
					addCandidate(base, "", toolsText != "", toolsText, mcpText != "", mcpText)
				}
			}
		}
	}

	if m.selection.Active {
		start, end := m.selection.Normalized()
		lines := end.Line - start.Line + 1
		if start.Line == end.Line && start.Col == end.Col {
			lines = 0
		}
		if lines > 0 {
			hintSeg := seg(mutedStyle.Render(fmt.Sprintf("%d lines · ctrl+y:copy", lines)), 60, false)
			for i := range candidates {
				candidates[i] = append(candidates[i], hintSeg)
			}
		}
	}
	if m.copyStatus != "" {
		copySeg := seg(mutedStyle.Render(m.copyStatus), 60, false)
		for i := range candidates {
			candidates[i] = append(candidates[i], copySeg)
		}
	}

	rightWidths := make([]int, len(rightVariants))
	for i, right := range rightVariants {
		rightWidths[i] = lipgloss.Width(right)
	}
	for ri, right := range rightVariants {
		for _, candidate := range candidates {
			line, ok := composeStatusLine(candidate, sep, sepWidth, right, rightWidths[ri], width)
			if ok {
				return line
			}
		}
	}

	right := rightVariants[len(rightVariants)-1]
	rightWidth := rightWidths[len(rightWidths)-1]
	if rightWidth >= width {
		return ansi.Cut(right, 0, width)
	}
	left := joinStatusSegments(dropStatusSegments(candidates[len(candidates)-1], width-rightWidth-1, sep, sepWidth), sep)
	line, ok := composeStatusLineText(left, right, width)
	if ok {
		return line
	}
	return ansi.Cut(line, 0, width)
}

func (m *Model) statusLineWorktreePart() string {
	if m == nil || m.sess == nil {
		return ""
	}
	dir := strings.TrimSpace(m.sess.WorktreeDir)
	if dir == "" {
		return ""
	}
	name := strings.TrimSpace(filepath.Base(filepath.Clean(dir)))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "worktree"
	}
	return "wt:" + name
}

func (m *Model) statusLineUsageParts() (string, string) {
	usageBase := ""
	if m.engine != nil && m.engine.InputLimit() > 0 {
		contextTokens := 0
		if !m.streaming {
			contextTokens = m.engine.LastTotalTokens()
		}
		if contextTokens <= 0 {
			contextTokens = m.estimateContextTokensCached()
		}
		limit := m.engine.InputLimit()
		if limit > 0 {
			used := llm.FormatTokenCount(contextTokens)
			if used == "" {
				used = "0"
			}
			usageBase = fmt.Sprintf("~%s/%s", used, llm.FormatTokenCount(limit))
		}
	}

	cachedInputTokens := 0
	if m.stats != nil && m.stats.CachedInputTokens > 0 {
		cachedInputTokens = m.stats.CachedInputTokens
	}
	if cachedInputTokens <= 0 {
		return usageBase, usageBase
	}
	cachedLabel := llm.FormatTokenCount(cachedInputTokens)
	if cachedLabel == "" {
		return usageBase, usageBase
	}
	longCache := fmt.Sprintf("%s cached", cachedLabel)
	shortCache := fmt.Sprintf("%s C", cachedLabel)
	if usageBase != "" {
		return fmt.Sprintf("%s (%s)", usageBase, longCache), fmt.Sprintf("%s (%s)", usageBase, shortCache)
	}
	return longCache, shortCache
}

func (m *Model) statusLineToolsParts(successStyle lipgloss.Style) (string, string) {
	if len(m.localTools) == 0 {
		return "", ""
	}
	shortText := fmt.Sprintf("tools:%d", len(m.localTools))
	if len(m.localTools) == len(tools.AllToolNames()) {
		shortText = "tools:all"
	}
	short := successStyle.Render(shortText)
	if len(m.localTools) >= 4 || shortText == "tools:all" {
		return short, short
	}
	full := successStyle.Render("tools:" + strings.Join(m.localTools, ","))
	return full, short
}

func (m *Model) statusLineMCPParts(successStyle, mutedStyle lipgloss.Style) (string, string) {
	if m.mcpManager == nil {
		return "", ""
	}
	available := m.mcpManager.AvailableServers()
	if len(available) == 0 {
		return "", ""
	}
	enabled := m.mcpManager.EnabledServers()
	if len(enabled) == 0 {
		off := mutedStyle.Render("mcp:off")
		return off, off
	}
	full := successStyle.Render("mcp:" + strings.Join(enabled, ","))
	short := successStyle.Render(fmt.Sprintf("mcp:%d", len(enabled)))
	return full, short
}

func (m *Model) statusLineStreamingVariants(mutedStyle lipgloss.Style) []string {
	if !m.streaming {
		return nil
	}
	elapsed := formatChatElapsed(time.Since(m.streamStartTime))
	spinnerPhase := strings.TrimSpace(m.spinner.View() + " " + m.phase)
	var variants []string
	if m.currentTokens > 0 {
		variants = append(variants, mutedStyle.Render(strings.Join([]string{spinnerPhase, fmt.Sprintf("%d tok", m.currentTokens), elapsed}, " ")))
	}
	variants = append(variants,
		mutedStyle.Render(strings.Join([]string{spinnerPhase, elapsed}, " ")),
		mutedStyle.Render(strings.Join([]string{m.phase, elapsed}, " ")),
		mutedStyle.Render(m.phase),
	)
	return variants
}

func statusSegmentVariants(segments []statusSegment) [][]statusSegment {
	variants := [][]statusSegment{append([]statusSegment(nil), segments...)}
	compact := append([]statusSegment(nil), segments...)
	for {
		dropIdx := -1
		lowestPriority := int(^uint(0) >> 1)
		for i, segment := range compact {
			if segment.essential {
				continue
			}
			if segment.priority < lowestPriority {
				lowestPriority = segment.priority
				dropIdx = i
			}
		}
		if dropIdx == -1 {
			break
		}
		compact = append(compact[:dropIdx], compact[dropIdx+1:]...)
		variants = append(variants, append([]statusSegment(nil), compact...))
	}
	return variants
}

func joinStatusSegments(segments []statusSegment, sep string) string {
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment.text != "" {
			parts = append(parts, segment.text)
		}
	}
	return strings.Join(parts, sep)
}

// statusSegmentsWidth returns the joined display width of segments using
// per-segment cached widths, measuring (and memoizing) any not yet measured.
// This keeps the candidate fitting search in renderStatusLine arithmetic
// instead of re-joining and ANSI-parsing strings for every combination.
func statusSegmentsWidth(segments []statusSegment, sepWidth int) (total, count int) {
	for i := range segments {
		if segments[i].text == "" {
			continue
		}
		if segments[i].width == 0 {
			segments[i].width = lipgloss.Width(segments[i].text)
		}
		total += segments[i].width
		count++
	}
	if count > 1 {
		total += sepWidth * (count - 1)
	}
	return total, count
}

func composeStatusLine(segments []statusSegment, sep string, sepWidth int, right string, rightWidth, width int) (string, bool) {
	leftWidth, count := statusSegmentsWidth(segments, sepWidth)
	if right == "" {
		if leftWidth <= width {
			return joinStatusSegments(segments, sep), true
		}
		return "", false
	}
	if rightWidth > width {
		return "", false
	}
	if count == 0 {
		return strings.Repeat(" ", width-rightWidth) + right, true
	}
	spaces := width - leftWidth - rightWidth
	if spaces < 1 {
		return "", false
	}
	return joinStatusSegments(segments, sep) + strings.Repeat(" ", spaces) + right, true
}

func composeStatusLineText(left, right string, width int) (string, bool) {
	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	if right == "" {
		if leftWidth <= width {
			return left, true
		}
		return "", false
	}
	if rightWidth > width {
		return "", false
	}
	if left == "" {
		return strings.Repeat(" ", width-rightWidth) + right, true
	}
	spaces := width - leftWidth - rightWidth
	if spaces < 1 {
		return "", false
	}
	return left + strings.Repeat(" ", spaces) + right, true
}

func dropStatusSegments(segments []statusSegment, maxWidth int, sep string, sepWidth int) []statusSegment {
	if maxWidth <= 0 {
		return nil
	}
	kept := append([]statusSegment(nil), segments...)
	for {
		total, _ := statusSegmentsWidth(kept, sepWidth)
		if total <= maxWidth {
			break
		}
		dropIdx := -1
		lowestPriority := int(^uint(0) >> 1)
		for i, segment := range kept {
			if segment.essential {
				continue
			}
			if segment.priority < lowestPriority {
				lowestPriority = segment.priority
				dropIdx = i
			}
		}
		if dropIdx == -1 {
			break
		}
		kept = append(kept[:dropIdx], kept[dropIdx+1:]...)
	}
	if total, _ := statusSegmentsWidth(kept, sepWidth); total > maxWidth {
		left := joinStatusSegments(kept, sep)
		return []statusSegment{{text: ansi.Cut(left, 0, maxWidth), essential: true}}
	}
	return kept
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
	lowerQuery := strings.ToLower(query)

	// While streaming, completion choices must be limited to slash commands that
	// are safe to execute locally. Keep /effort argument completions available,
	// but do not suggest commands such as /model that would mutate stream state.
	if m.streaming && strings.HasPrefix(lowerQuery, "effort ") {
		parts := strings.SplitN(query, " ", 2)
		partial := ""
		if len(parts) == 2 {
			partial = parts[1]
		}
		m.completions.SetItems(m.effortCompletionItems(parts[0]+" ", partial))
		return
	}
	if m.streaming && strings.Contains(lowerQuery, " ") {
		m.completions.Hide()
		return
	}

	if items, ok := shellCompletionItems(query); ok {
		m.completions.SetItems(items)
		return
	}

	if items, ok := m.worktreeCompletionItems(query); ok {
		m.completions.SetItems(items)
		return
	}

	// Check for MCP server argument completions
	// /mcp start <server>, /mcp stop <server>, /mcp add <server>

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

	// Check for "/effort " - show reasoning effort completions for current model.
	if strings.HasPrefix(lowerQuery, "effort ") {
		parts := strings.SplitN(query, " ", 2)
		partial := ""
		if len(parts) == 2 {
			partial = parts[1]
		}
		m.completions.SetItems(m.effortCompletionItems(parts[0]+" ", partial))
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

	// Default: use standard command filtering. During streaming, only suggest
	// commands that are handled locally instead of sent as interjections.
	if m.streaming {
		items := FilterCommands(query)
		local := make([]Command, 0, len(items))
		for _, item := range items {
			if isStreamingLocalSlashCommand("/" + item.Name) {
				local = append(local, item)
			}
		}
		m.completions.SetItems(local)
		return
	}
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
		Mode:                        mode,
		Width:                       m.width,
		Height:                      m.height,
		ReasoningExpansionOverrides: m.reasoningExpansionOverrides,
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

func (m *Model) renderAltScreenStreamingContentLines(streamingContent string) []string {
	return appendViewportContentLines(slices.Clone(m.viewCache.historyLines), streamingContent)
}

// renderAltScreenViewportLines renders the chat viewport directly from pre-wrapped
// content lines. It's only used for wave-only tool animation frames, where the
// visible text changes but the underlying viewport content/line count does not.
func (m *Model) renderAltScreenViewportLines(lines []string) string {
	w, h := m.viewport.Width(), m.viewport.Height()
	if sw := m.viewport.Style.GetWidth(); sw != 0 && sw < w {
		w = sw
	}
	if sh := m.viewport.Style.GetHeight(); sh != 0 && sh < h {
		h = sh
	}
	if w == 0 || h == 0 {
		return ""
	}

	contentWidth := w - m.viewport.Style.GetHorizontalFrameSize()
	contentHeight := h - m.viewport.Style.GetVerticalFrameSize()
	if contentWidth < 0 {
		contentWidth = 0
	}
	if contentHeight < 0 {
		contentHeight = 0
	}

	yOffset := m.viewport.YOffset()
	if yOffset < 0 {
		yOffset = 0
	}
	if yOffset > len(lines) {
		yOffset = len(lines)
	}
	bottom := yOffset + contentHeight
	if bottom > len(lines) {
		bottom = len(lines)
	}

	visible := slices.Clone(lines[yOffset:bottom])
	if len(m.viewportImageBlocks) > 0 && m.usePostFrameImageComposition() {
		m.beginPostFrameImageComposition()
		m.overlayVisibleViewportImages(visible, yOffset)
		m.finishPostFrameImageComposition()
	} else {
		m.overlayVisibleViewportImages(visible, yOffset)
	}
	if xOffset := m.viewport.XOffset(); xOffset > 0 && contentWidth > 0 {
		for i := range visible {
			visible[i] = ansi.Cut(visible[i], xOffset, xOffset+contentWidth)
		}
	}

	// Over-wide lines must be truncated, never wrapped: lipgloss Width() wraps,
	// which inflates the frame beyond the viewport height and pushes the footer's
	// status line past the terminal bottom where Bubble Tea clips it.
	// viewport.View() truncates, so this path must match.
	if contentWidth > 0 {
		for i := range visible {
			if lipgloss.Width(visible[i]) > contentWidth {
				visible[i] = ansi.Cut(visible[i], 0, contentWidth)
			}
		}
	}

	rendered := lipgloss.NewStyle().
		Width(contentWidth).
		Height(contentHeight).
		Render(strings.Join(visible, "\n"))
	return m.viewport.Style.
		UnsetWidth().UnsetHeight().
		Render(rendered)
}

func (m *Model) renderAltScreenViewportLinesWithImages(lines []string) string {
	return m.renderAltScreenViewportLines(lines)
}

func (m *Model) overlayVisibleViewportImages(visible []string, yOffset int) {
	if m == nil || len(visible) == 0 || len(m.viewportImageBlocks) == 0 || m.imagePlaceholdersSuppressed {
		return
	}
	viewportBottom := yOffset + len(visible)
	for _, block := range m.viewportImageBlocks {
		art, ok := m.viewportImageArtifacts[block.Key]
		if !ok {
			continue
		}
		blockBottom := block.StartLine + block.HeightCells
		if blockBottom <= yOffset || block.StartLine >= viewportBottom {
			continue
		}

		visibleTop := max(block.StartLine, yOffset)
		visibleBottom := min(blockBottom, viewportBottom)
		sliceStart := visibleTop - block.StartLine
		sliceRows := visibleBottom - visibleTop
		if sliceRows <= 0 {
			continue
		}

		displayArt := art
		if art.Path != "" && m.usePostFrameImageComposition() {
			m.queuePostFrameViewportImage(art, block.StartLine, sliceStart, sliceRows, visibleTop-yOffset)
			continue
		} else if art.Path != "" {
			var rendered bool
			displayArt, rendered = m.renderViewportImageSliceArtifact(art, sliceStart, sliceRows)
			if !rendered {
				continue
			}
		} else if len(displayArt.Rows) == 0 {
			continue
		}

		uploadKey := m.viewportImageUploadKey(displayArt.Key)
		if displayArt.Upload != "" {
			if m.uploadedImageKeys == nil {
				m.uploadedImageKeys = make(map[string]struct{})
			}
			if _, uploaded := m.uploadedImageKeys[uploadKey]; !uploaded {
				m.queueImageUpload(uploadKey, displayArt.Upload)
				// Keep the reserved blank rows from the backing viewport content.
				// Placeholders are injected only after the upload has been flushed.
				continue
			}
		}
		placeKey := uploadKey + ":place"
		if displayArt.Place != "" {
			if m.placedImageKeys == nil {
				m.placedImageKeys = make(map[string]struct{})
			}
			if _, placed := m.placedImageKeys[placeKey]; !placed {
				m.queueImagePlacement(placeKey, displayArt.Place)
				// The virtual placement must be acknowledged by our flush bookkeeping
				// before placeholders enter the viewport, so resize/redraw ordering cannot
				// bind a new image to stale placeholder cells.
				continue
			}
		}

		rows := displayArt.Rows
		if art.Path == "" && sliceStart > 0 {
			if sliceStart >= len(rows) {
				continue
			}
			rows = rows[sliceStart:]
		}
		for i := 0; i < sliceRows && i < len(rows); i++ {
			screenRow := visibleTop + i - yOffset
			if screenRow < 0 || screenRow >= len(visible) {
				continue
			}
			row := rows[i]
			if width := lipgloss.Width(row); width < m.viewport.Width() {
				row += strings.Repeat(" ", m.viewport.Width()-width)
			}
			visible[screenRow] = row
		}
	}
}

// tryAppendAltScreenStreamingContent reuses the previously rendered viewport
// lines only when that viewport is known to be exactly historyContent plus the
// previous streaming tail. Call resetAltScreenStreamingAppendCache whenever
// history, width, or turn state changes so stale history cannot be retained by
// this append-only fast path.
func (m *Model) tryAppendAltScreenStreamingContent(streamingContent string) ([]string, bool) {
	if !m.viewCache.lastContentHistoryPlusStream {
		return nil, false
	}
	if !strings.HasPrefix(streamingContent, m.viewCache.lastStreamingContent) {
		return nil, false
	}

	contentLines := m.contentLines
	if contentLines == nil {
		contentLines = splitViewportContentLines(m.viewCache.lastContentStr)
	}
	if contentLines == nil && m.viewCache.lastContentStr == "" {
		return nil, false
	}

	delta := streamingContent[len(m.viewCache.lastStreamingContent):]
	return appendViewportContentLines(contentLines, delta), true
}

func splitViewportContentLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func appendViewportContentLines(lines []string, delta string) []string {
	if len(lines) == 0 {
		return splitViewportContentLines(delta)
	}
	if delta == "" {
		return lines
	}

	parts := strings.Split(delta, "\n")
	lines[len(lines)-1] += parts[0]
	if len(parts) > 1 {
		lines = append(lines, parts[1:]...)
	}
	return lines
}

// GetBundledServers returns bundled MCP servers (wrapper for mcp package)
func GetBundledServers() []mcp.BundledServer {
	return mcp.GetBundledServers()
}
