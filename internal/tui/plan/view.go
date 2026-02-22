package plan

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/ui"
)

// View renders the UI.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Render ask_user UI if active
	if m.askUserModel != nil {
		// Show document above ask_user
		b.WriteString(m.renderDocument())
		b.WriteString("\n")
		b.WriteString(m.askUserModel.View())
		// Pad to push to bottom
		rendered := b.String()
		lineCount := strings.Count(rendered, "\n")
		if gap := m.height - 1 - lineCount; gap > 0 {
			b.WriteString(strings.Repeat("\n", gap))
		}
		return b.String()
	}

	// Recalculate editor height for activity panel
	m.recalcEditorHeight()

	// Main content: editor with visual selection highlighting
	editorView := m.editor.View()
	if m.visualMode {
		editorView = m.applyVisualHighlight(editorView)
	}
	b.WriteString(editorView)
	b.WriteString("\n")

	// Calculate footer height: activity panel + separator + optional pending + chat + separator + status
	footerLines := 4 // separator + chat + separator + status
	if len(m.pendingPrompts) > 0 {
		footerLines++ // pending prompts indicator
	}
	footerLines += m.activityPanelHeight()
	if m.activityPanelHeight() > 0 {
		footerLines++ // trailing newline after panel
	}

	// Pad gap between editor and footer to push everything to bottom
	rendered := b.String()
	lineCount := strings.Count(rendered, "\n")
	if gap := m.height - lineCount - footerLines; gap > 0 {
		b.WriteString(strings.Repeat("\n", gap))
	}

	// Activity panel (right above chat)
	if panel := m.renderActivityPanel(); panel != "" {
		b.WriteString(panel)
		b.WriteString("\n")
	}

	// Separator line above chat section
	separator := strings.Repeat("─", m.width)
	b.WriteString(m.styles.Muted.Render(separator))
	b.WriteString("\n")

	// Pending prompts indicator
	if pending := m.renderPendingPrompts(); pending != "" {
		b.WriteString(pending)
		b.WriteString("\n")
	}

	// Chat input
	b.WriteString(m.renderChatInput())
	b.WriteString("\n")

	// Separator line below chat section
	b.WriteString(m.styles.Muted.Render(separator))
	b.WriteString("\n")

	// Status line
	b.WriteString(m.renderStatusLine())

	result := b.String()

	// Overlay agent picker if visible
	if m.agentPickerVisible {
		result = m.overlayBox(result, m.renderAgentPicker())
	}

	// Overlay help if visible
	if m.helpVisible {
		result = m.overlayBox(result, m.renderHelpOverlay())
	}

	return result
}

// applyVisualHighlight applies reverse video highlighting to selected lines in visual mode.
func (m *Model) applyVisualHighlight(editorView string) string {
	lines := strings.Split(editorView, "\n")

	startLine := min(m.visualStart, m.visualEnd)
	endLine := max(m.visualStart, m.visualEnd)

	// Apply highlight style to selected lines
	highlightStyle := lipgloss.NewStyle().Reverse(true)

	for i := startLine; i <= endLine && i < len(lines); i++ {
		if i >= 0 {
			lines[i] = highlightStyle.Render(lines[i])
		}
	}

	return strings.Join(lines, "\n")
}

// renderPendingPrompts renders the pending prompt queue indicator.
func (m *Model) renderPendingPrompts() string {
	if len(m.pendingPrompts) == 0 {
		return ""
	}

	theme := m.styles.Theme()
	style := lipgloss.NewStyle().Foreground(theme.Warning)

	preview := truncateResult(m.pendingPrompts[0], 50)
	if len(m.pendingPrompts) > 1 {
		return style.Render(fmt.Sprintf("Queued: %q (+%d more)", preview, len(m.pendingPrompts)-1))
	}
	return style.Render(fmt.Sprintf("Queued: %q", preview))
}

// renderChatInput renders the chat input area.
func (m *Model) renderChatInput() string {
	theme := m.styles.Theme()

	// Border style based on focus
	var prefix string
	if m.chatFocused {
		prefix = lipgloss.NewStyle().Foreground(theme.Primary).Render("> ")
	} else {
		prefix = lipgloss.NewStyle().Foreground(theme.Muted).Render("> ")
	}

	// Render the textarea (single line)
	inputView := m.chatInput.View()

	// Add hint when not focused
	if !m.chatFocused && m.chatInput.Value() == "" {
		hint := lipgloss.NewStyle().Foreground(theme.Muted).Render("(Tab or click to focus chat)")
		return prefix + hint
	}

	return prefix + inputView
}

// chatInputY returns the Y coordinate of the chat input line on screen.
func (m *Model) chatInputY() int {
	// From bottom: status (1) + separator (1) + chat (1) = chat is at height-3
	// Plus optional pending prompts line above chat
	y := m.height - 3
	if len(m.pendingPrompts) > 0 {
		y--
	}
	return y
}

// recalcEditorHeight recalculates the editor height accounting for activity panel.
func (m *Model) recalcEditorHeight() {
	m.editor.SetWidth(m.width - 4)
	panelH := m.activityPanelHeight()
	// Fixed overhead: 2 separators + 1 chat input + 1 status line + editor borders (~4) = 8
	overhead := 8
	if len(m.pendingPrompts) > 0 {
		overhead++ // pending prompts indicator line
	}
	editorH := m.height - overhead - panelH
	if editorH < 3 {
		editorH = 3
	}
	m.editor.SetHeight(editorH)
}

// maxVisibleTools is the maximum number of tool lines shown in the activity panel.
// When more tools exist, a summary line "(N more tool calls)" is shown instead.
const maxVisibleTools = 3

// activityPanelHeight returns the number of lines the activity panel uses.
// Returns 0 when the panel should not be shown.
func (m *Model) activityPanelHeight() int {
	if !m.agentActive && m.stats == nil {
		return 0
	}
	if !m.activityExpanded {
		return 0
	}

	// Count: top separator (1) + tool lines + reasoning line + bottom separator (1)
	lines := 2 // top + bottom separator

	// Count total tool segments (completed + active)
	totalTools := 0
	if m.tracker != nil {
		for i := range m.tracker.Segments {
			if m.tracker.Segments[i].Type == ui.SegmentTool {
				totalTools++
			}
		}
	}

	// Show up to maxVisibleTools, plus a summary line if there are more
	if totalTools <= maxVisibleTools {
		lines += totalTools
	} else {
		lines += maxVisibleTools + 1 // visible tools + "(N more)" line
	}

	// Reasoning text line (if any)
	if m.agentLastReasoningLn != "" {
		lines++
	}

	return lines
}

// renderActivityPanel renders the agent activity panel between editor and chat.
func (m *Model) renderActivityPanel() string {
	if !m.agentActive && m.stats == nil {
		return ""
	}

	theme := m.styles.Theme()
	separator := strings.Repeat("─", m.width)
	mutedSep := m.styles.Muted.Render(separator)

	var b strings.Builder

	// Top separator with label
	label := " Agent "
	if m.width > len(label)+4 {
		leftDash := strings.Repeat("─", 2)
		rightDash := strings.Repeat("─", m.width-len(label)-2)
		b.WriteString(m.styles.Muted.Render(leftDash))
		b.WriteString(lipgloss.NewStyle().Foreground(theme.Primary).Render(label))
		b.WriteString(m.styles.Muted.Render(rightDash))
	} else {
		b.WriteString(mutedSep)
	}
	b.WriteString("\n")

	if !m.activityExpanded {
		return ""
	}

	// Collect all tool segments (completed then active)
	var completedTools []*ui.Segment
	var activeTools []*ui.Segment
	if m.tracker != nil {
		for i := range m.tracker.Segments {
			seg := &m.tracker.Segments[i]
			if seg.Type == ui.SegmentTool {
				if seg.ToolStatus == ui.ToolPending {
					activeTools = append(activeTools, seg)
				} else {
					completedTools = append(completedTools, seg)
				}
			}
		}
	}

	// Show the most recent tools (up to maxVisibleTools), with overflow summary
	allTools := append(completedTools, activeTools...)
	totalTools := len(allTools)
	if totalTools > maxVisibleTools {
		hidden := totalTools - maxVisibleTools
		b.WriteString(m.styles.Muted.Render(fmt.Sprintf("  (%d more tool calls)", hidden)))
		b.WriteString("\n")
		allTools = allTools[totalTools-maxVisibleTools:]
	}
	for _, seg := range allTools {
		wavePos := -1
		if seg.ToolStatus == ui.ToolPending {
			wavePos = m.tracker.WavePos
		}
		b.WriteString(ui.RenderToolSegment(seg, wavePos, m.width, false))
		b.WriteString("\n")
	}

	// Last line of agent reasoning text (dimmed)
	if m.agentLastReasoningLn != "" {
		lastLine := truncateRunesWithEllipsis(m.agentLastReasoningLn, max(0, m.width-4))
		b.WriteString(m.styles.Muted.Render("  " + lastLine))
		b.WriteString("\n")
	}

	// Bottom separator
	b.WriteString(mutedSep)

	return b.String()
}

func truncateRunesWithEllipsis(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}

	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}

	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}

	return string(runes[:maxRunes-3]) + "..."
}

func (m *Model) renderDocument() string {
	var b strings.Builder
	lines := m.doc.Lines()

	for _, line := range lines {
		// Indicate agent-edited lines with a subtle marker
		if line.Author == "agent" {
			b.WriteString(m.styles.Muted.Render("│ "))
		} else {
			b.WriteString("  ")
		}
		b.WriteString(line.Content)
		b.WriteString("\n")
	}

	return b.String()
}

func (m *Model) renderStatusLine() string {
	theme := m.styles.Theme()

	// Command mode: show command line
	if m.commandMode {
		cmdLine := ":" + m.commandBuffer + "_"
		return lipgloss.NewStyle().Foreground(theme.Primary).Render(cmdLine)
	}

	// Left side: vim mode indicator
	var vimIndicator string
	if m.chatFocused {
		vimIndicator = "-- CHAT --"
	} else if m.visualMode {
		vimIndicator = "-- VISUAL LINE --"
	} else if m.vimMode {
		vimIndicator = "-- NORMAL --"
	} else {
		vimIndicator = "-- INSERT --"
	}

	// Second part: agent status with inline stats
	var status string
	if m.agentActive {
		var parts []string
		parts = append(parts, m.spinner.View()+" "+m.agentPhase)
		if m.currentTurn > 1 {
			parts = append(parts, fmt.Sprintf("turn %d", m.currentTurn))
		}
		if m.stats != nil && m.stats.ToolCallCount > 0 {
			if m.stats.ToolCallCount == 1 {
				parts = append(parts, "1 tool")
			} else {
				parts = append(parts, fmt.Sprintf("%d tools", m.stats.ToolCallCount))
			}
		}
		if m.stats != nil {
			totalTok := m.stats.InputTokens + m.stats.OutputTokens
			if totalTok > 0 {
				parts = append(parts, ui.FormatTokenCount(totalTok)+" tok")
			}
		}
		if !m.streamStartTime.IsZero() {
			parts = append(parts, fmt.Sprintf("%.1fs", time.Since(m.streamStartTime).Seconds()))
		}
		status = strings.Join(parts, " | ")
	} else if m.statusMsg != "" && time.Since(m.statusMsgTime) < 5*time.Second {
		status = m.statusMsg
	}

	// Build left side: vim indicator (primary) + status (dim)
	leftStr := lipgloss.NewStyle().Foreground(theme.Primary).Render(vimIndicator)
	if status != "" {
		leftStr += "  " + lipgloss.NewStyle().Foreground(theme.Muted).Render(status)
	}

	// Middle: document info
	var middle string
	lineCount := m.doc.LineCount()
	if lineCount == 1 {
		middle = "1 line"
	} else {
		middle = fmt.Sprintf("%d lines", lineCount)
	}
	if len(m.history) > 0 {
		turns := len(m.history) / 2
		if turns == 1 {
			middle += " | 1 turn"
		} else {
			middle += fmt.Sprintf(" | %d turns", turns)
		}
	}
	if len(m.pendingPrompts) > 0 {
		middle += fmt.Sprintf(" | %d queued", len(m.pendingPrompts))
	}

	// Right side: compact shortcuts (full list via Ctrl+H help)
	var right string
	if m.agentActive {
		right = "Esc: cancel  ^H: help"
	} else if m.chatFocused {
		right = "Enter: send  ^H: help"
	} else {
		right = "^P: plan  ^H: help"
	}

	// Build status line
	middleStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	rightStyle := lipgloss.NewStyle().Foreground(theme.Muted)

	middleStr := middleStyle.Render(middle)
	rightStr := rightStyle.Render(right)

	// Calculate padding to distribute space
	leftWidth := lipgloss.Width(leftStr)
	middleWidth := lipgloss.Width(middleStr)
	rightWidth := lipgloss.Width(rightStr)
	totalContent := leftWidth + middleWidth + rightWidth
	availableSpace := m.width - totalContent

	if availableSpace < 2 {
		// Not enough space - just show left and right
		padding := max(1, m.width-leftWidth-rightWidth)
		return leftStr + strings.Repeat(" ", padding) + rightStr
	}

	// Split padding between left-middle and middle-right
	leftPad := availableSpace / 2
	rightPad := availableSpace - leftPad

	return leftStr + strings.Repeat(" ", leftPad) + middleStr + strings.Repeat(" ", rightPad) + rightStr
}
