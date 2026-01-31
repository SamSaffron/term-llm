package chat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
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
func (m *Model) View() string {
	if m.quitting {
		return ""
	}

	// Inspector mode uses alternate screen
	if m.inspectorMode && m.inspectorModel != nil {
		return m.inspectorModel.View()
	}

	// Set terminal title
	title := m.getTerminalTitle()
	titleSeq := fmt.Sprintf("\x1b]0;%s\x07", title)

	// Alt screen mode: use viewport for scrollable content
	if m.altScreen {
		return titleSeq + m.viewAltScreen()
	}

	// Auto-send mode: minimal rendering for benchmarking (skip expensive UI)
	if m.autoSendQueue != nil {
		return titleSeq + m.viewAutoSend()
	}

	// Inline mode: traditional rendering
	var b strings.Builder

	// History (if scrolling)
	if m.scrollOffset > 0 {
		b.WriteString(m.renderHistory())
		b.WriteString("\n")
	}

	// Streaming response (if active)
	if m.streaming {
		b.WriteString(m.renderStreamingInline())
	}

	// Error display (if error occurred and not streaming)
	if m.err != nil && !m.streaming {
		b.WriteString(m.renderError())
		b.WriteString("\n\n")
	}

	// Completions popup (if visible)
	if m.completions.IsVisible() {
		b.WriteString(m.completions.View())
		b.WriteString("\n")
	}

	// Dialog (if open)
	if m.dialog.IsOpen() {
		b.WriteString(m.dialog.View())
		b.WriteString("\n")
	}

	// Input prompt
	b.WriteString(m.renderInputInline())

	return titleSeq + b.String()
}

// viewAltScreen renders the full-screen alt screen view with scrollable viewport
func (m *Model) viewAltScreen() string {
	var b strings.Builder

	// Build scrollable content with caching to avoid re-rendering unchanged content

	// Check if history cache is valid
	historyValid := m.viewCache.historyValid &&
		m.viewCache.historyMsgCount == len(m.messages) &&
		m.viewCache.historyWidth == m.width &&
		m.viewCache.historyScrollOffset == m.scrollOffset
	if !historyValid {
		m.viewCache.historyContent = m.renderHistory()
		m.viewCache.historyMsgCount = len(m.messages)
		m.viewCache.historyWidth = m.width
		m.viewCache.historyScrollOffset = m.scrollOffset
		m.viewCache.historyValid = true
		m.viewCache.contentVersion++ // History changed
	}

	// Build combined content
	var contentStr string
	if m.streaming {
		// During streaming, combine history + streaming content
		streamingContent := m.renderStreamingInline()
		contentStr = m.viewCache.historyContent + streamingContent

		// If an embedded UI is active, append it to the content
		if m.approvalModel != nil {
			contentStr += "\n" + m.approvalModel.View()
		} else if m.askUserModel != nil {
			contentStr += "\n" + m.askUserModel.View()
		}
		// Only increment version when tracker content or wave position changes
		// The completed segment rendering is cached, but we still need SetContent
		// for wave animation updates
		if m.tracker != nil {
			trackerVersion := m.tracker.Version
			wavePos := m.tracker.WavePos
			contentChanged := trackerVersion != m.viewCache.lastTrackerVersion
			waveChanged := wavePos != m.viewCache.lastWavePos && m.tracker.HasPending()
			if contentChanged || waveChanged {
				m.viewCache.lastTrackerVersion = trackerVersion
				m.viewCache.lastWavePos = wavePos
				m.viewCache.contentVersion++
			}
		}
	} else {
		// Include any completed streaming content (diffs, tools) that was saved
		// when the last stream ended. This persists until a new prompt is sent.
		contentStr = m.viewCache.historyContent + m.viewCache.completedStream

		// Display error if one occurred
		if m.err != nil {
			contentStr += "\n" + m.renderError() + "\n"
		}
	}

	// Only call SetContent if content actually changed (expensive operation)
	// Use version comparison instead of O(n) string comparison
	contentChanged := m.viewCache.contentVersion != m.viewCache.lastRenderedVersion

	// Force update if embedded UI is active (since it's interactive and doesn't affect tracker version)
	if m.approvalModel != nil || m.askUserModel != nil {
		contentChanged = true
	}

	if contentChanged {
		// Check if user is at bottom BEFORE setting content (which changes maxYOffset)
		wasAtBottom := m.viewport.AtBottom()
		m.viewport.SetContent(contentStr)
		m.viewCache.lastRenderedVersion = m.viewCache.contentVersion
		// Only auto-scroll to bottom if user was already at bottom
		// This allows users to scroll up and read during streaming
		// GotoBottom() is expensive as it calls visibleLines() internally
		if m.streaming && wasAtBottom {
			m.viewport.GotoBottom()
		}
	}

	// Scroll to bottom after response completes (regardless of previous scroll position)
	if m.scrollToBottom {
		m.viewport.GotoBottom()
		m.scrollToBottom = false
	}

	// Cache viewport.View() output - only regenerate if content, scroll position, or size changed
	// Check YOffset after GotoBottom() since it modifies the offset
	yOffsetChanged := m.viewport.YOffset != m.viewCache.lastYOffset
	sizeChanged := m.viewport.Width != m.viewCache.lastVPWidth || m.viewport.Height != m.viewCache.lastVPHeight
	needViewRender := contentChanged || yOffsetChanged || sizeChanged || m.viewCache.lastViewportView == ""
	if needViewRender {
		m.viewCache.lastViewportView = m.viewport.View()
		m.viewCache.lastYOffset = m.viewport.YOffset
		m.viewCache.lastVPWidth = m.viewport.Width
		m.viewCache.lastVPHeight = m.viewport.Height
	}

	// Render viewport (scrollable area)
	b.WriteString(m.viewCache.lastViewportView)
	b.WriteString("\n")

	// Completions popup (if visible) - overlaid on content
	if m.completions.IsVisible() {
		b.WriteString(m.completions.View())
		b.WriteString("\n")
	}

	// Dialog (if open) - overlaid on content
	if m.dialog.IsOpen() {
		b.WriteString(m.dialog.View())
		b.WriteString("\n")
	}

	// Input area (fixed at bottom)
	b.WriteString(m.renderInputInline())

	return b.String()
}

// viewAutoSend renders a minimal view for auto-send benchmarking mode.
// This skips expensive UI elements like textarea, separators, and status line
// to measure pure LLM response time without rendering overhead.
func (m *Model) viewAutoSend() string {
	if m.streaming {
		// Minimal status line during streaming
		elapsed := time.Since(m.streamStartTime)
		return fmt.Sprintf("%s:%s · mcp:off · %s  Responding %.1fs",
			m.providerName, m.modelName, m.spinner.View(), elapsed.Seconds())
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
		hasContent := b.Len() > 0
		if hasContent {
			b.WriteString("\n\n")
		}

		wavePos := 0
		var active []*ui.Segment
		if m.tracker != nil {
			wavePos = m.tracker.WavePos
			active = m.tracker.ActiveSegments()
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
	theme := m.styles.Theme()

	var b strings.Builder

	// Separator line above input (no extra newline - content already has one)
	separator := lipgloss.NewStyle().Foreground(theme.Muted).Render(strings.Repeat("─", m.width))
	b.WriteString(separator)

	// Show attached files if any
	if len(m.files) > 0 {
		b.WriteString("\n")
		var fileNames []string
		for _, f := range m.files {
			fileNames = append(fileNames, f.Name)
		}
		filesInfo := lipgloss.NewStyle().Foreground(theme.Secondary).Render(
			fmt.Sprintf("[with: %s]", strings.Join(fileNames, ", ")))
		b.WriteString(filesInfo)
	}

	// Input prompt
	b.WriteString("\n")
	b.WriteString(m.textarea.View())
	b.WriteString("\n")

	// Separator line below input
	b.WriteString(separator)
	b.WriteString("\n")

	// Status line
	b.WriteString(m.renderStatusLine())

	return b.String()
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

// renderStatusLine renders a tiny status line showing model and options
func (m *Model) renderStatusLine() string {
	theme := m.styles.Theme()
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	successStyle := lipgloss.NewStyle().Foreground(theme.Success)

	const sep = " · "

	// Build fixed parts first (these are always shown as-is)
	var fixedParts []string

	// Provider:model format (e.g. "chatgpt:gpt-5.2-codex")
	model := shortenModelName(m.modelName)
	if model == "" {
		fixedParts = append(fixedParts, m.providerName)
	} else {
		fixedParts = append(fixedParts, fmt.Sprintf("%s:%s", m.providerName, model))
	}

	// Web search status
	if m.searchEnabled {
		fixedParts = append(fixedParts, successStyle.Render("web:on"))
	}

	// File count if any
	if len(m.files) > 0 {
		fixedParts = append(fixedParts, fmt.Sprintf("%d file(s)", len(m.files)))
	}

	// During streaming, add progress info
	var streamingPart string
	if m.streaming {
		elapsed := time.Since(m.streamStartTime)
		progressParts := []string{m.spinner.View() + " " + m.phase}
		if m.currentTokens > 0 {
			progressParts = append(progressParts, fmt.Sprintf("%d tok", m.currentTokens))
		}
		progressParts = append(progressParts, fmt.Sprintf("%.1fs", elapsed.Seconds()))
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

	// Build tools string - use full names if they fit, otherwise abbreviate
	var toolsPart string
	if len(m.localTools) > 0 {
		if len(m.localTools) == len(tools.AllToolNames()) {
			toolsPart = successStyle.Render("tools:all")
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
		} else if len(m.messages) == 0 {
			// Show hint for new users on empty conversation
			mcpPart = mutedStyle.Render("Ctrl+T:mcp")
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

	return mutedStyle.Render(strings.Join(parts, sep))
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

// updateCompletions updates the completions popup based on current input
// Handles both static command completions and dynamic server completions
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

	// In text mode, skip markdown rendering but still apply word wrapping
	if m.textMode {
		targetWidth := m.width - 2
		if targetWidth < 20 {
			targetWidth = 20
		}
		return wordwrap.String(content, targetWidth)
	}

	targetWidth := m.width - 2

	// Reuse cached renderer if width matches
	if m.rendererCache.renderer != nil && m.rendererCache.width == targetWidth {
		rendered, err := m.rendererCache.renderer.Render(content)
		if err != nil {
			return content
		}
		return strings.TrimSpace(rendered)
	}

	// Create new renderer and cache it
	style := ui.GlamourStyle()
	margin := uint(0)
	style.Document.Margin = &margin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = &margin

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(targetWidth),
	)
	if err != nil {
		return content
	}

	m.rendererCache.renderer = renderer
	m.rendererCache.width = targetWidth

	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}

	return strings.TrimSpace(rendered)
}

// copyLastResponse copies the last assistant response to clipboard
func (m *Model) copyLastResponse() (string, error) {
	// Find last assistant message
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == llm.RoleAssistant {
			content := m.messages[i].TextContent
			// Try to copy to clipboard using OSC 52 escape sequence
			// This works in most modern terminals
			if err := copyToClipboard(content); err != nil {
				return "", err
			}
			return "Copied last response to clipboard.", nil
		}
	}
	return "", fmt.Errorf("no assistant response to copy")
}

// clipboardCopiedMsg signals clipboard copy completed
type clipboardCopiedMsg struct{}

// maxClipboardSize is the maximum size for OSC52 clipboard operations.
// Many terminals have limits; 100KB is conservative and works with most.
const maxClipboardSize = 100 * 1024

// copyToClipboard uses OSC 52 escape sequence.
// Returns an error if the text exceeds maxClipboardSize.
func copyToClipboard(text string) error {
	if len(text) > maxClipboardSize {
		return fmt.Errorf("content too large for clipboard (%d bytes, max %d)", len(text), maxClipboardSize)
	}
	// OSC 52 clipboard escape sequence
	// Works in terminals that support it (iTerm2, Ghostty, kitty, etc.)
	encoded := base64Encode(text)
	fmt.Printf("\x1b]52;c;%s\x07", encoded)
	return nil
}

// base64Encode encodes text for clipboard
func base64Encode(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

// GetBundledServers returns bundled MCP servers (wrapper for mcp package)
func GetBundledServers() []mcp.BundledServer {
	return mcp.GetBundledServers()
}
