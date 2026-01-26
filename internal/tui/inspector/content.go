package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

// maxContentLines is the maximum lines to show before truncating with expand option
const maxContentLines = 40

// ContentItem tracks a truncatable content block in the rendered output
type ContentItem struct {
	ID          string // e.g., "msg-0", "tool-call_123"
	ItemType    string // "message", "tool_call", "tool_result"
	StartLine   int    // Start line in rendered content
	EndLine     int    // End line (exclusive)
	IsTruncated bool
	TotalLines  int // Original line count before truncation
}

// ContentRenderer handles rendering of conversation messages
type ContentRenderer struct {
	width       int
	styles      *ui.Styles
	expandedIDs map[string]bool // IDs of items that should be expanded (not truncated)
	store       session.Store   // Session store for fetching subagent messages
}

// NewContentRenderer creates a new content renderer
func NewContentRenderer(width int, styles *ui.Styles, expandedIDs map[string]bool, store session.Store) *ContentRenderer {
	if expandedIDs == nil {
		expandedIDs = make(map[string]bool)
	}
	return &ContentRenderer{
		width:       width,
		styles:      styles,
		expandedIDs: expandedIDs,
		store:       store,
	}
}

// RenderMessages renders all messages into a string and returns content items for truncation tracking
func (r *ContentRenderer) RenderMessages(messages []session.Message) (string, []ContentItem) {
	var b strings.Builder
	var items []ContentItem
	currentLine := 0

	for i, msg := range messages {
		if i > 0 {
			b.WriteString("\n")
			currentLine++
		}

		msgID := fmt.Sprintf("msg-%d", i)
		content, msgItems := r.renderMessageWithItems(msg, msgID, currentLine)
		b.WriteString(content)
		items = append(items, msgItems...)
		currentLine += strings.Count(content, "\n")
	}
	return b.String(), items
}

// renderMessage renders a single message with its parts (backward compat, no item tracking)
func (r *ContentRenderer) renderMessage(msg session.Message) string {
	content, _ := r.renderMessageWithItems(msg, "", 0)
	return content
}

// renderMessageWithItems renders a single message and tracks content items for truncation
func (r *ContentRenderer) renderMessageWithItems(msg session.Message, msgID string, startLine int) (string, []ContentItem) {
	theme := r.styles.Theme()
	var b strings.Builder
	var items []ContentItem

	// Role header
	roleStyle := lipgloss.NewStyle().Bold(true)
	switch msg.Role {
	case llm.RoleUser:
		roleStyle = roleStyle.Foreground(theme.Primary)
		content, item := r.renderBoxWithItem("User", roleStyle, r.renderUserContent(msg), msgID, "message", startLine)
		b.WriteString(content)
		if item != nil {
			items = append(items, *item)
		}
	case llm.RoleAssistant:
		roleStyle = roleStyle.Foreground(theme.Secondary)
		content, assistantItems := r.renderAssistantContentWithItems(msg, msgID, startLine)
		boxContent, item := r.renderBoxWithItem("Assistant", roleStyle, content, msgID, "message", startLine)
		b.WriteString(boxContent)
		if item != nil {
			items = append(items, *item)
		}
		items = append(items, assistantItems...)
	case llm.RoleSystem:
		roleStyle = roleStyle.Foreground(theme.Warning)
		content, item := r.renderBoxWithItem("System", roleStyle, r.renderTextContent(msg), msgID, "message", startLine)
		b.WriteString(content)
		if item != nil {
			items = append(items, *item)
		}
	case llm.RoleTool:
		content, toolItems := r.renderToolResultsWithItems(msg, msgID, startLine)
		b.WriteString(content)
		items = append(items, toolItems...)
	}

	return b.String(), items
}

// renderBox creates a box around content with a header
func (r *ContentRenderer) renderBox(header string, headerStyle lipgloss.Style, content string) string {
	boxContent, _ := r.renderBoxWithItem(header, headerStyle, content, "", "", 0)
	return boxContent
}

// renderBoxWithItem creates a box with truncation tracking
func (r *ContentRenderer) renderBoxWithItem(header string, headerStyle lipgloss.Style, content string, itemID string, itemType string, startLine int) (string, *ContentItem) {
	theme := r.styles.Theme()
	borderColor := theme.Border

	// Calculate inner width (accounting for borders and padding)
	innerWidth := max(r.width-4, 20)

	var b strings.Builder
	lineCount := 0

	// Top border with header
	topLeft := lipgloss.NewStyle().Foreground(borderColor).Render("╭─ ")
	headerText := headerStyle.Render(header)
	headerWidth := lipgloss.Width(topLeft) + lipgloss.Width(headerText) + 1
	remainingDashes := max(r.width-headerWidth-1, 0)
	topRight := lipgloss.NewStyle().Foreground(borderColor).Render(strings.Repeat("─", remainingDashes) + "╮")

	b.WriteString(topLeft)
	b.WriteString(headerText)
	b.WriteString(" ")
	b.WriteString(topRight)
	b.WriteString("\n")
	lineCount++

	// Content lines with borders
	lines := strings.Split(content, "\n")

	// Count total lines after wrapping (for accurate truncation info)
	var wrappedLineCount int
	for _, line := range lines {
		wrappedLineCount += len(wrapLine(line, innerWidth))
	}

	// Check if we need to truncate
	isExpanded := r.expandedIDs[itemID]
	isTruncated := false
	truncatedLines := 0

	// Apply truncation if not expanded and content is too long
	if !isExpanded && wrappedLineCount > maxContentLines {
		isTruncated = true
		truncatedLines = wrappedLineCount - maxContentLines
	}

	borderChar := lipgloss.NewStyle().Foreground(borderColor).Render("│")
	renderedContentLines := 0
	for _, line := range lines {
		// Wrap long lines
		wrappedLines := wrapLine(line, innerWidth)
		for _, wl := range wrappedLines {
			// Check if we should truncate
			if isTruncated && renderedContentLines >= maxContentLines {
				break
			}

			padding := max(innerWidth-lipgloss.Width(wl), 0)
			b.WriteString(borderChar)
			b.WriteString(" ")
			b.WriteString(wl)
			b.WriteString(strings.Repeat(" ", padding))
			b.WriteString(" ")
			b.WriteString(borderChar)
			b.WriteString("\n")
			lineCount++
			renderedContentLines++
		}
		if isTruncated && renderedContentLines >= maxContentLines {
			break
		}
	}

	// Add truncation indicator if needed
	if isTruncated {
		truncateMsg := fmt.Sprintf("... (%d more lines, press 'e' to expand)", truncatedLines)
		truncateStyle := lipgloss.NewStyle().Foreground(theme.Secondary).Italic(true)
		padding := max(innerWidth-lipgloss.Width(truncateMsg), 0)
		b.WriteString(borderChar)
		b.WriteString(" ")
		b.WriteString(truncateStyle.Render(truncateMsg))
		b.WriteString(strings.Repeat(" ", padding))
		b.WriteString(" ")
		b.WriteString(borderChar)
		b.WriteString("\n")
		lineCount++
	}

	// Bottom border (clamp width to avoid panic on very small terminals)
	borderWidth := max(r.width, 2)
	bottomBorder := lipgloss.NewStyle().Foreground(borderColor).Render("╰" + strings.Repeat("─", borderWidth-2) + "╯")
	b.WriteString(bottomBorder)

	// Create content item if we have an ID
	var item *ContentItem
	if itemID != "" {
		item = &ContentItem{
			ID:          itemID,
			ItemType:    itemType,
			StartLine:   startLine,
			EndLine:     startLine + lineCount,
			IsTruncated: isTruncated,
			TotalLines:  wrappedLineCount + 2, // +2 for header and footer
		}
	}

	return b.String(), item
}

// wrapLine wraps a single line to fit within maxWidth
func wrapLine(line string, maxWidth int) []string {
	// Convert tabs to spaces first - lipgloss.Width counts tabs as 1,
	// but terminals render them as multiple columns (typically 8).
	// Use 2 spaces per tab for compact display.
	line = strings.ReplaceAll(line, "\t", "  ")

	if maxWidth <= 0 {
		return []string{line}
	}
	if lipgloss.Width(line) <= maxWidth {
		return []string{line}
	}

	var result []string
	words := strings.Fields(line)
	if len(words) == 0 {
		return []string{""}
	}

	current := words[0]
	for _, word := range words[1:] {
		test := current + " " + word
		if lipgloss.Width(test) <= maxWidth {
			current = test
		} else {
			result = append(result, current)
			current = word
		}
	}
	result = append(result, current)
	return result
}

// renderUserContent renders user message content
func (r *ContentRenderer) renderUserContent(msg session.Message) string {
	return r.renderTextContent(msg)
}

// renderTextContent extracts and renders text content from a message
func (r *ContentRenderer) renderTextContent(msg session.Message) string {
	var texts []string
	for _, part := range msg.Parts {
		if part.Type == llm.PartText && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	if len(texts) == 0 {
		return "(empty)"
	}
	return strings.Join(texts, "\n")
}

// renderAssistantContent renders assistant message content including tool calls
func (r *ContentRenderer) renderAssistantContent(msg session.Message) string {
	content, _ := r.renderAssistantContentWithItems(msg, "", 0)
	return content
}

// renderAssistantContentWithItems renders assistant content and tracks tool call items
func (r *ContentRenderer) renderAssistantContentWithItems(msg session.Message, msgID string, startLine int) (string, []ContentItem) {
	var b strings.Builder
	var items []ContentItem
	var hasText bool
	currentLine := startLine

	for i, part := range msg.Parts {
		switch part.Type {
		case llm.PartText:
			if part.Text != "" {
				if hasText {
					b.WriteString("\n")
					currentLine++
				}
				b.WriteString(part.Text)
				currentLine += strings.Count(part.Text, "\n")
				hasText = true
			}
		case llm.PartToolCall:
			if part.ToolCall != nil {
				if b.Len() > 0 {
					b.WriteString("\n\n")
					currentLine += 2
				}
				itemID := fmt.Sprintf("%s-tc-%d-%s", msgID, i, part.ToolCall.ID)
				content, item := r.renderToolCallWithItem(part.ToolCall, itemID, currentLine)
				b.WriteString(content)
				if item != nil {
					items = append(items, *item)
				}
				currentLine += strings.Count(content, "\n")
			}
		}
	}

	if b.Len() == 0 {
		return "(empty)", items
	}
	return b.String(), items
}

// renderToolCall renders a tool call with its arguments
func (r *ContentRenderer) renderToolCall(tc *llm.ToolCall) string {
	content, _ := r.renderToolCallWithItem(tc, "", 0)
	return content
}

// renderToolCallWithItem renders a tool call with truncation tracking
func (r *ContentRenderer) renderToolCallWithItem(tc *llm.ToolCall, itemID string, startLine int) (string, *ContentItem) {
	theme := r.styles.Theme()
	var b strings.Builder
	lineCount := 0

	// Tool call header
	headerStyle := lipgloss.NewStyle().Foreground(theme.Secondary).Bold(true)
	idStyle := lipgloss.NewStyle().Foreground(theme.Muted)

	b.WriteString(lipgloss.NewStyle().Foreground(theme.Border).Render("┌ "))
	b.WriteString(headerStyle.Render("Tool Call: "))
	b.WriteString(lipgloss.NewStyle().Foreground(theme.Primary).Render(tc.Name))
	b.WriteString(" ")
	b.WriteString(idStyle.Render("[" + truncateID(tc.ID) + "]"))
	b.WriteString("\n")
	lineCount++

	// Arguments as pretty-printed JSON
	args := r.formatJSON(tc.Arguments)
	innerWidth := max(r.width-6, 20)
	borderStyle := lipgloss.NewStyle().Foreground(theme.Border)

	// Count total lines after wrapping
	var wrappedLineCount int
	for line := range strings.SplitSeq(args, "\n") {
		wrappedLineCount += len(wrapLine(line, innerWidth))
	}

	// Check if we need to truncate
	isExpanded := r.expandedIDs[itemID]
	isTruncated := false
	truncatedLines := 0

	if !isExpanded && wrappedLineCount > maxContentLines {
		isTruncated = true
		truncatedLines = wrappedLineCount - maxContentLines
	}

	renderedContentLines := 0
	for line := range strings.SplitSeq(args, "\n") {
		// Wrap long lines
		wrappedLines := wrapLine(line, innerWidth)
		for _, wl := range wrappedLines {
			if isTruncated && renderedContentLines >= maxContentLines {
				break
			}
			b.WriteString(borderStyle.Render("│ "))
			b.WriteString(wl)
			b.WriteString("\n")
			lineCount++
			renderedContentLines++
		}
		if isTruncated && renderedContentLines >= maxContentLines {
			break
		}
	}

	// Add truncation indicator if needed
	if isTruncated {
		truncateMsg := fmt.Sprintf("... (%d more lines, press 'e' to expand)", truncatedLines)
		truncateStyle := lipgloss.NewStyle().Foreground(theme.Secondary).Italic(true)
		b.WriteString(borderStyle.Render("│ "))
		b.WriteString(truncateStyle.Render(truncateMsg))
		b.WriteString("\n")
		lineCount++
	}

	b.WriteString(lipgloss.NewStyle().Foreground(theme.Border).Render("└"))

	// Create content item if we have an ID
	var item *ContentItem
	if itemID != "" {
		item = &ContentItem{
			ID:          itemID,
			ItemType:    "tool_call",
			StartLine:   startLine,
			EndLine:     startLine + lineCount,
			IsTruncated: isTruncated,
			TotalLines:  wrappedLineCount + 2, // +2 for header and footer
		}
	}

	return b.String(), item
}

// renderToolResults renders tool result messages
func (r *ContentRenderer) renderToolResults(msg session.Message) string {
	content, _ := r.renderToolResultsWithItems(msg, "", 0)
	return content
}

// renderToolResultsWithItems renders tool results with truncation tracking
func (r *ContentRenderer) renderToolResultsWithItems(msg session.Message, msgID string, startLine int) (string, []ContentItem) {
	theme := r.styles.Theme()
	var b strings.Builder
	var items []ContentItem
	currentLine := startLine

	for i, part := range msg.Parts {
		if part.Type == llm.PartToolResult && part.ToolResult != nil {
			tr := part.ToolResult
			itemID := fmt.Sprintf("%s-tr-%d-%s", msgID, i, tr.ID)
			lineCount := 0

			// Tool result header
			headerStyle := lipgloss.NewStyle().Foreground(theme.Secondary).Bold(true)
			idStyle := lipgloss.NewStyle().Foreground(theme.Muted)

			statusIcon := lipgloss.NewStyle().Foreground(theme.Success).Render("✓")
			if tr.IsError {
				statusIcon = lipgloss.NewStyle().Foreground(theme.Error).Render("✗")
			}

			b.WriteString(lipgloss.NewStyle().Foreground(theme.Border).Render("┌ "))
			b.WriteString(headerStyle.Render("Tool Result: "))
			b.WriteString(lipgloss.NewStyle().Foreground(theme.Primary).Render(tr.Name))
			b.WriteString(" ")
			b.WriteString(idStyle.Render("[" + truncateID(tr.ID) + "]"))
			b.WriteString(" ")
			b.WriteString(statusIcon)
			b.WriteString("\n")
			lineCount++

			// Content
			content := tr.Content
			lines := strings.Split(content, "\n")

			innerWidth := max(r.width-6, 20)
			borderStyle := lipgloss.NewStyle().Foreground(theme.Border)

			// Count total lines after wrapping
			var wrappedLineCount int
			for _, line := range lines {
				wrappedLineCount += len(wrapLine(line, innerWidth))
			}

			// Check if we need to truncate
			isExpanded := r.expandedIDs[itemID]
			isTruncated := false
			truncatedLines := 0

			if !isExpanded && wrappedLineCount > maxContentLines {
				isTruncated = true
				truncatedLines = wrappedLineCount - maxContentLines
			}

			renderedContentLines := 0
			for _, line := range lines {
				// Wrap long lines instead of truncating
				wrappedLines := wrapLine(line, innerWidth)
				for _, wl := range wrappedLines {
					if isTruncated && renderedContentLines >= maxContentLines {
						break
					}
					b.WriteString(borderStyle.Render("│ "))
					b.WriteString(wl)
					b.WriteString("\n")
					lineCount++
					renderedContentLines++
				}
				if isTruncated && renderedContentLines >= maxContentLines {
					break
				}
			}

			// Add truncation indicator if needed
			if isTruncated {
				truncateMsg := fmt.Sprintf("... (%d more lines, press 'e' to expand)", truncatedLines)
				truncateStyle := lipgloss.NewStyle().Foreground(theme.Secondary).Italic(true)
				b.WriteString(borderStyle.Render("│ "))
				b.WriteString(truncateStyle.Render(truncateMsg))
				b.WriteString("\n")
				lineCount++
			}

			// Show total size if content is large
			if len(content) > 1000 {
				b.WriteString(lipgloss.NewStyle().Foreground(theme.Border).Render("│ "))
				b.WriteString(idStyle.Render(fmt.Sprintf("(%d bytes total)", len(content))))
				b.WriteString("\n")
				lineCount++
			}

			b.WriteString(lipgloss.NewStyle().Foreground(theme.Border).Render("└"))
			b.WriteString("\n")

			// Create content item
			if itemID != "" {
				items = append(items, ContentItem{
					ID:          itemID,
					ItemType:    "tool_result",
					StartLine:   currentLine,
					EndLine:     currentLine + lineCount,
					IsTruncated: isTruncated,
					TotalLines:  wrappedLineCount + 2, // +2 for header and footer
				})
			}

			currentLine += lineCount + 1 // +1 for the bottom border line

			// Check if this is a spawn_agent result with a session_id
			if tr.Name == "spawn_agent" {
				if sessionID := extractSessionIDFromSpawnAgentResult(content); sessionID != "" {
					// Render the subagent's internal turns
					subagentContent, subagentItems := r.renderSubagentSession(sessionID, currentLine)
					if subagentContent != "" {
						b.WriteString(subagentContent)
						items = append(items, subagentItems...)
						currentLine += strings.Count(subagentContent, "\n")
					}
				}
			}
		}
	}

	return strings.TrimSuffix(b.String(), "\n"), items
}

// formatJSON formats JSON with indentation
func (r *ContentRenderer) formatJSON(data json.RawMessage) string {
	if len(data) == 0 {
		return "{}"
	}

	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		// If it's not valid JSON, return as-is
		return string(data)
	}

	formatted, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return string(data)
	}
	return string(formatted)
}

// truncateID shortens a tool call ID for display
func truncateID(id string) string {
	if len(id) <= 16 {
		return id
	}
	// Show first 8 and last 4 characters
	return id[:8] + "..." + id[len(id)-4:]
}

// extractSessionIDFromSpawnAgentResult extracts the session_id from spawn_agent result JSON
func extractSessionIDFromSpawnAgentResult(content string) string {
	// Try to parse as JSON and extract session_id
	var result struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return ""
	}
	return result.SessionID
}

// renderSubagentSession renders the internal turns of a subagent session
func (r *ContentRenderer) renderSubagentSession(sessionID string, startLine int) (string, []ContentItem) {
	if r.store == nil || sessionID == "" {
		return "", nil
	}

	theme := r.styles.Theme()
	var b strings.Builder
	var items []ContentItem
	currentLine := startLine

	// Fetch session messages
	ctx := context.Background()
	messages, err := r.store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil || len(messages) == 0 {
		return "", nil
	}

	// Header for subagent section
	headerStyle := lipgloss.NewStyle().
		Foreground(theme.Muted).
		Bold(true).
		Italic(true)

	borderColor := theme.Muted
	innerWidth := max(r.width-6, 20)

	b.WriteString("\n")
	currentLine++
	b.WriteString(lipgloss.NewStyle().Foreground(borderColor).Render("  ┌─ "))
	b.WriteString(headerStyle.Render("Subagent Internal Turns"))
	b.WriteString(lipgloss.NewStyle().Foreground(borderColor).Render(" ─────"))
	b.WriteString("\n")
	currentLine++

	// Render each message - use readable text color for content, muted for borders
	textStyle := lipgloss.NewStyle().Foreground(theme.Text)
	roleStyle := lipgloss.NewStyle().Foreground(theme.Secondary)
	borderStyle := lipgloss.NewStyle().Foreground(borderColor)

	for i, msg := range messages {
		msgID := fmt.Sprintf("subagent-%s-msg-%d", sessionID, i)

		// Determine role label
		var roleLabel string
		switch msg.Role {
		case llm.RoleUser:
			roleLabel = "User"
		case llm.RoleAssistant:
			roleLabel = "Assistant"
		case llm.RoleSystem:
			roleLabel = "System"
		case llm.RoleTool:
			roleLabel = "Tool"
		default:
			roleLabel = string(msg.Role)
		}

		// Role header within subagent section
		b.WriteString(borderStyle.Render("  │ "))
		b.WriteString(roleStyle.Render("─── " + roleLabel + " ───"))
		b.WriteString("\n")
		currentLine++

		// Render message content based on type
		var contentText string
		for _, part := range msg.Parts {
			switch part.Type {
			case llm.PartText:
				if part.Text != "" {
					contentText += part.Text
				}
			case llm.PartToolCall:
				if part.ToolCall != nil {
					contentText += fmt.Sprintf("[Tool Call: %s]", part.ToolCall.Name)
				}
			case llm.PartToolResult:
				if part.ToolResult != nil {
					status := "✓"
					if part.ToolResult.IsError {
						status = "✗"
					}
					// Truncate long results
					resultText := part.ToolResult.Content
					if len(resultText) > 200 {
						resultText = resultText[:200] + "..."
					}
					contentText += fmt.Sprintf("[Tool Result: %s %s]\n%s", part.ToolResult.Name, status, resultText)
				}
			}
		}

		if contentText == "" {
			contentText = "(empty)"
		}

		// Apply truncation to subagent content as well
		lines := strings.Split(contentText, "\n")

		// Count total lines after wrapping
		var wrappedLineCount int
		for _, line := range lines {
			wrappedLineCount += len(wrapLine(line, innerWidth))
		}

		isExpanded := r.expandedIDs[msgID]
		isTruncated := false
		truncatedLines := 0

		if !isExpanded && wrappedLineCount > maxContentLines {
			isTruncated = true
			truncatedLines = wrappedLineCount - maxContentLines
		}

		itemStartLine := currentLine
		renderedLines := 0
		for _, line := range lines {
			wrappedLines := wrapLine(line, innerWidth)
			for _, wl := range wrappedLines {
				if isTruncated && renderedLines >= maxContentLines {
					break
				}
				b.WriteString(borderStyle.Render("  │   "))
				b.WriteString(textStyle.Render(wl))
				b.WriteString("\n")
				currentLine++
				renderedLines++
			}
			if isTruncated && renderedLines >= maxContentLines {
				break
			}
		}

		// Add truncation indicator if needed
		if isTruncated {
			truncateMsg := fmt.Sprintf("... (%d more lines, press 'e' to expand)", truncatedLines)
			truncateStyle := lipgloss.NewStyle().Foreground(theme.Secondary).Italic(true)
			b.WriteString(borderStyle.Render("  │   "))
			b.WriteString(truncateStyle.Render(truncateMsg))
			b.WriteString("\n")
			currentLine++
		}

		// Track this subagent message as a content item
		items = append(items, ContentItem{
			ID:          msgID,
			ItemType:    "subagent_message",
			StartLine:   itemStartLine,
			EndLine:     currentLine,
			IsTruncated: isTruncated,
			TotalLines:  wrappedLineCount,
		})
	}

	// Footer
	b.WriteString(lipgloss.NewStyle().Foreground(borderColor).Render("  └───────────────────────"))
	currentLine++

	return b.String(), items
}
