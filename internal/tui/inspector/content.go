package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	appconfig "github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

// maxContentLines is the maximum lines to show before truncating with expand option
const maxContentLines = 40

// ContentItem tracks a truncatable content block in the rendered output
type ContentItem struct {
	ID          string // e.g., "msg-0", "tool-call_123"
	ItemType    string // e.g., "message", "tool_call", "tool_result", "tool_definition", "compaction"
	StartLine   int    // Start line in rendered content
	EndLine     int    // End line (exclusive)
	IsTruncated bool
	TotalLines  int // Original line count before truncation
}

// ContentRenderer handles rendering of conversation messages
type ContentRenderer struct {
	width           int
	styles          *ui.Styles
	expandedIDs     map[string]bool // IDs of items that should be expanded (not truncated)
	store           session.Store   // Session store for fetching subagent messages
	providerName    string
	modelName       string
	toolSpecs       []llm.ToolSpec
	toolDiscovery   *llm.ToolDiscoveryDiagnostics
	reasoningConfig appconfig.ReasoningConfig

	hasCompactionBoundary   bool
	compactionBoundaryIndex int
	compactionBoundarySeq   int
	compactionCount         int
}

// NewContentRenderer creates a new content renderer
func NewContentRenderer(width int, styles *ui.Styles, expandedIDs map[string]bool, store session.Store, providerName, modelName string, toolSpecs []llm.ToolSpec, reasoningCfg appconfig.ReasoningConfig) *ContentRenderer {
	if expandedIDs == nil {
		expandedIDs = make(map[string]bool)
	}
	return &ContentRenderer{
		width:           width,
		styles:          styles,
		expandedIDs:     expandedIDs,
		store:           store,
		providerName:    providerName,
		modelName:       modelName,
		toolSpecs:       toolSpecs,
		reasoningConfig: reasoningCfg,
	}
}

// SetToolDiscovery configures the provider-neutral discovery diagnostics shown
// in the inspector header.
func (r *ContentRenderer) SetToolDiscovery(diagnostics *llm.ToolDiscoveryDiagnostics) {
	r.toolDiscovery = diagnostics
}

// SetCompactionBoundary configures an optional marker for the latest active
// context boundary. The renderer also detects every persisted compaction
// summary message by content so older compactions remain visible in Ctrl+O.
func (r *ContentRenderer) SetCompactionBoundary(hasBoundary bool, index, seq, count int) {
	r.hasCompactionBoundary = hasBoundary
	r.compactionBoundaryIndex = index
	r.compactionBoundarySeq = seq
	r.compactionCount = count
}

// renderHeader renders the model information and system message section
func (r *ContentRenderer) renderHeader(messages []session.Message) (string, []ContentItem, int) {
	theme := r.styles.Theme()
	var b strings.Builder
	var items []ContentItem
	currentLine := 0

	// Model Information section (only if we have provider/model info)
	if r.providerName != "" || r.modelName != "" {
		headerStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Primary)
		labelStyle := lipgloss.NewStyle().Foreground(theme.Muted)
		valueStyle := lipgloss.NewStyle().Foreground(theme.Text)

		b.WriteString(headerStyle.Render("Model Information"))
		b.WriteString("\n")
		currentLine++

		if r.providerName != "" {
			b.WriteString(labelStyle.Render("  Provider: "))
			b.WriteString(valueStyle.Render(r.providerName))
			b.WriteString("\n")
			currentLine++
		}
		if r.modelName != "" {
			b.WriteString(labelStyle.Render("  Model: "))
			b.WriteString(valueStyle.Render(r.modelName))
			b.WriteString("\n")
			currentLine++
		}
		b.WriteString("\n")
		currentLine++
	}

	if discovery := r.toolDiscovery; discovery != nil {
		headerStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Secondary)
		labelStyle := lipgloss.NewStyle().Foreground(theme.Muted)
		valueStyle := lipgloss.NewStyle().Foreground(theme.Text)
		writeField := func(label, value string) {
			b.WriteString(labelStyle.Render("  " + label + ": "))
			b.WriteString(valueStyle.Render(value))
			b.WriteString("\n")
			currentLine++
		}
		b.WriteString(headerStyle.Render("Tool Discovery"))
		b.WriteString("\n")
		currentLine++
		writeField("Mode configured/resolved", fmt.Sprintf("%s / %s", discovery.ConfiguredMode, discovery.ResolvedMode))
		writeField("Strategy configured/resolved", fmt.Sprintf("%s / %s", discovery.ConfiguredStrategy, discovery.Strategy))
		writeField("Mode reason", discovery.Reason)
		writeField("Strategy reason", discovery.StrategyReason)
		if discovery.FallbackCount > 0 {
			writeField("Native fallback", fmt.Sprintf("%d (%s)", discovery.FallbackCount, discovery.FallbackReason))
		}
		writeField("Pinned MCP", fmt.Sprintf("%d tools, ~%d tokens", discovery.PinnedCount, discovery.PinnedTokens))
		writeField("Active MCP", fmt.Sprintf("%d tools, ~%d tokens", discovery.ActiveMCPCount, discovery.ActiveMCPTokens))
		writeField("Deferred MCP", fmt.Sprintf("%d tools, ~%d tokens avoided", discovery.DeferredCount, discovery.DeferredTokens))
		writeField("Dynamic budget", fmt.Sprintf("%d/%d tools", discovery.DynamicActive, discovery.DynamicLimit))
		if len(discovery.Recent) > 0 {
			b.WriteString(labelStyle.Render("  Recent activation:\n"))
			currentLine++
			for _, activation := range discovery.Recent {
				identity := activation.Name
				if activation.Namespace != "" && activation.ChildName != "" {
					identity = fmt.Sprintf("%s/%s (%s)", activation.Namespace, activation.ChildName, activation.Name)
				}
				b.WriteString(valueStyle.Render(fmt.Sprintf("    %s — %s", identity, activation.Reason)))
				b.WriteString("\n")
				currentLine++
			}
		}
		b.WriteString("\n")
		currentLine++
	}

	// System Message section - extract first system message
	var systemMsg *session.Message
	for i := range messages {
		if messages[i].Role == llm.RoleSystem {
			systemMsg = &messages[i]
			break
		}
	}

	if systemMsg != nil {
		headerStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Warning)
		b.WriteString(headerStyle.Render("System Prompt"))
		b.WriteString("\n")
		currentLine++

		// Render system message content in a box
		content := r.renderTextContent(*systemMsg)
		boxContent, item := r.renderBoxWithItem("System", lipgloss.NewStyle().Foreground(theme.Warning).Bold(true), content, "system-msg", "message", currentLine)
		b.WriteString(boxContent)
		if item != nil {
			items = append(items, *item)
			// Use the item's EndLine for accurate line tracking (accounts for bottom border without newline)
			currentLine = item.EndLine
		} else {
			// Fallback: count newlines + 1 for bottom border
			currentLine += strings.Count(boxContent, "\n") + 1
		}
		b.WriteString("\n")
		// This newline only terminates the system box's bottom-border line. The
		// next rendered section starts on currentLine; it is not an extra blank
		// content line unless another newline is written before content follows.
	}

	return b.String(), items, currentLine
}

// renderToolDefinitions renders the tool definitions section
func (r *ContentRenderer) renderToolDefinitions() (string, []ContentItem, int) {
	if len(r.toolSpecs) == 0 {
		return "", nil, 0
	}

	theme := r.styles.Theme()
	var b strings.Builder
	var items []ContentItem
	currentLine := 0

	// Section header
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Secondary)
	b.WriteString(headerStyle.Render(fmt.Sprintf("Tool Definitions (%d tools)", len(r.toolSpecs))))
	b.WriteString("\n")
	currentLine++

	// List each tool
	nameStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Primary)
	descStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	borderStyle := lipgloss.NewStyle().Foreground(theme.Border)
	schemaStyle := lipgloss.NewStyle().Foreground(theme.Text)

	for i, tool := range r.toolSpecs {
		itemID := fmt.Sprintf("tool-def-%d", i)
		itemStartLine := currentLine

		// Tool name
		b.WriteString(borderStyle.Render("  • "))
		b.WriteString(nameStyle.Render(tool.Name))

		// Truncate description to ~100 chars
		desc := tool.Description
		if len(desc) > 100 {
			desc = desc[:97] + "..."
		}
		if desc != "" {
			b.WriteString(descStyle.Render(" - " + desc))
		}

		hasSchema := len(tool.Schema) > 0
		isExpanded := r.expandedIDs[itemID]

		// Show expand hint if schema exists but not expanded
		if hasSchema && !isExpanded {
			b.WriteString(descStyle.Render(" [press 'e' to show schema]"))
		}
		b.WriteString("\n")
		currentLine++

		// Render schema if expanded
		if hasSchema && isExpanded {
			schemaJSON, err := json.MarshalIndent(tool.Schema, "", "  ")
			if err == nil {
				schemaLines := strings.Split(string(schemaJSON), "\n")
				for _, line := range schemaLines {
					b.WriteString(borderStyle.Render("      "))
					b.WriteString(schemaStyle.Render(line))
					b.WriteString("\n")
					currentLine++
				}
			}
		}

		// Track as expandable item for schema viewing
		items = append(items, ContentItem{
			ID:          itemID,
			ItemType:    "tool_definition",
			StartLine:   itemStartLine,
			EndLine:     currentLine,
			IsTruncated: hasSchema && !isExpanded, // Only truncated if has schema and not expanded
			TotalLines:  currentLine - itemStartLine,
		})
	}

	b.WriteString("\n")
	currentLine++

	return b.String(), items, currentLine
}

// RenderMessages renders all messages into a string and returns content items for truncation tracking
func (r *ContentRenderer) RenderMessages(messages []session.Message) (string, []ContentItem) {
	var b strings.Builder
	var items []ContentItem
	currentLine := 0

	// Render header (model info + system message)
	header, headerItems, headerLines := r.renderHeader(messages)
	if header != "" {
		b.WriteString(header)
		items = append(items, headerItems...)
		currentLine += headerLines
	}

	// Render tool definitions
	toolDefs, toolItems, toolLines := r.renderToolDefinitions()
	if toolDefs != "" {
		b.WriteString(toolDefs)
		items = append(items, toolItems...)
		currentLine += toolLines
	}

	// Add separator before conversation if we have header content
	if header != "" || toolDefs != "" {
		theme := r.styles.Theme()
		headerStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Text)
		b.WriteString(headerStyle.Render("Conversation"))
		b.WriteString("\n")
		currentLine++
		b.WriteString(lipgloss.NewStyle().Foreground(theme.Border).Render(strings.Repeat("─", r.width)))
		b.WriteString("\n")
		currentLine++
	}

	// Render messages (skip system messages since shown in header)
	firstBlock := true
	compactionSummaryOrdinal := 0
	boundaryMarkerIndex := r.compactionBoundaryMarkerIndex(messages)
	beginBlock := func() {
		if !firstBlock {
			// The separator newline terminates the previous block's last line and
			// positions the next block at the current line number; it does not add
			// an additional blank visual line.
			b.WriteString("\n")
		}
		firstBlock = false
	}
	writeBlock := func(content string, lineCount int) {
		if content == "" {
			return
		}
		beginBlock()
		b.WriteString(content)
		currentLine += lineCount
	}
	for i := 0; i < len(messages); {
		msg := messages[i]
		if isInspectorCompactionSummaryMessage(msg) {
			compactionSummaryOrdinal++
			isActiveBoundary := boundaryMarkerIndex == i
			beginBlock()
			block, item, lineCount, next := r.renderCompactionDebugBlock(messages, i, compactionSummaryOrdinal, isActiveBoundary, currentLine)
			b.WriteString(block)
			currentLine += lineCount
			if item != nil {
				items = append(items, *item)
			}
			i = next
			continue
		}

		isActiveBoundary := boundaryMarkerIndex == i
		if isActiveBoundary {
			marker := r.renderCompactionMarker(0, true, false)
			writeBlock(marker, visualLineCount(marker))
		}

		// Skip system messages - they're shown in the header
		if msg.Role == llm.RoleSystem {
			i++
			continue
		}

		msgID := fmt.Sprintf("msg-%d", i)
		beginBlock()
		content, msgItems, lineCount := r.renderMessageWithItems(msg, msgID, currentLine)
		b.WriteString(content)
		currentLine += lineCount
		items = append(items, msgItems...)
		i++
	}
	return b.String(), items
}

func (r *ContentRenderer) compactionBoundaryMarkerIndex(messages []session.Message) int {
	if !r.hasCompactionBoundary || len(messages) == 0 {
		return -1
	}
	idx := -1
	if r.compactionBoundarySeq >= 0 {
		for i, msg := range messages {
			if msg.Sequence >= r.compactionBoundarySeq {
				idx = i
				break
			}
		}
	}
	if idx < 0 && r.compactionBoundaryIndex >= 0 && r.compactionBoundaryIndex < len(messages) {
		idx = r.compactionBoundaryIndex
	}
	if idx < 0 {
		return -1
	}
	for idx < len(messages) && messages[idx].Role == llm.RoleSystem {
		idx++
	}
	if idx >= len(messages) {
		return -1
	}
	return idx
}

func (r *ContentRenderer) renderCompactionMarker(number int, activeBoundary, summaryFollows bool) string {
	theme := r.styles.Theme()
	label := "Context compaction"
	if number <= 0 && r.compactionCount > 0 {
		number = r.compactionCount
	}
	if number > 0 {
		label = fmt.Sprintf("%s #%d", label, number)
	}
	details := make([]string, 0, 2)
	if activeBoundary {
		details = append(details, "active context starts here")
	}
	if summaryFollows {
		details = append(details, "summary + retained turns available in debug block")
	}
	if len(details) == 0 {
		details = append(details, "compaction boundary")
	}
	text := "── " + label + " · " + strings.Join(details, " · ") + " ──"
	style := lipgloss.NewStyle().Foreground(theme.Muted).Italic(true)
	lines := wrapLine(text, max(r.width, 20))
	for i, line := range lines {
		lines[i] = style.Render(line)
	}
	return strings.Join(lines, "\n")
}

func (r *ContentRenderer) renderCompactionDebugBlock(messages []session.Message, summaryIdx, ordinal int, activeBoundary bool, startLine int) (string, *ContentItem, int, int) {
	next := compactionDebugTailEnd(messages, summaryIdx)
	summary := messages[summaryIdx]
	tail := messages[summaryIdx+1 : next]
	itemID := fmt.Sprintf("compaction-%d", summaryIdx)
	expanded := r.expandedIDs[itemID]

	marker := r.renderCompactionMarker(ordinal, activeBoundary, true)
	markerLines := visualLineCount(marker)

	content := r.renderCollapsedCompactionDebug(summary, tail, activeBoundary)
	if expanded {
		content = r.renderExpandedCompactionDebug(summary, tail, activeBoundary)
	}

	theme := r.styles.Theme()
	header := "Compaction Debug"
	if ordinal > 0 {
		header = fmt.Sprintf("Compaction Debug #%d", ordinal)
	}
	if expanded {
		header += " (expanded)"
	} else {
		header += " (collapsed)"
	}
	headerStyle := lipgloss.NewStyle().Foreground(theme.Muted).Bold(true)
	boxStart := startLine + markerLines
	box, boxItem := r.renderBoxWithItem(header, headerStyle, content, itemID, "compaction", boxStart)

	block := marker + "\n" + box
	lineCount := visualLineCount(block)
	if boxItem == nil {
		return block, nil, lineCount, next
	}
	item := *boxItem
	item.StartLine = startLine
	item.EndLine = startLine + lineCount
	item.IsTruncated = !expanded || item.IsTruncated
	item.TotalLines = max(item.TotalLines+markerLines, lineCount)
	return block, &item, lineCount, next
}

func compactionDebugTailEnd(messages []session.Message, summaryIdx int) int {
	end := summaryIdx + 1
	for end < len(messages) && messages[end].CompactionTail {
		end++
	}
	return end
}

func (r *ContentRenderer) renderCollapsedCompactionDebug(summary session.Message, tail []session.Message, activeBoundary bool) string {
	var b strings.Builder
	b.WriteString("Boundary: ")
	if activeBoundary {
		b.WriteString("active model context starts at this summary")
	} else {
		b.WriteString("historical compaction point")
	}
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("Summary row: %s, %d lines", formatCompactionDebugMessageRef(summary), textLineCount(compactionDebugMessageText(summary))))
	b.WriteString("\n")
	if len(tail) > 0 {
		b.WriteString(fmt.Sprintf("Retained raw tail: %d message%s hidden from normal chat but still sent to the model", len(tail), pluralSuffix(len(tail))))
	} else {
		b.WriteString("Retained raw tail: none recorded after this summary")
	}
	b.WriteString("\n")
	b.WriteString("Press 'e' anywhere in the inspector to expand all hidden debug details.")
	return b.String()
}

func (r *ContentRenderer) renderExpandedCompactionDebug(summary session.Message, tail []session.Message, activeBoundary bool) string {
	var b strings.Builder
	b.WriteString("Boundary\n")
	if activeBoundary {
		b.WriteString("- active model context starts here\n")
	} else {
		b.WriteString("- historical compaction point; a later compaction may define the current active boundary\n")
	}
	b.WriteString("- summary row: ")
	b.WriteString(formatCompactionDebugMessageRef(summary))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("- retained raw tail rows: %d\n", len(tail)))
	b.WriteString("\n")

	b.WriteString("Compacted summary\n")
	b.WriteString(strings.TrimRight(compactionDebugMessageText(summary), "\n"))
	if len(tail) == 0 {
		b.WriteString("\n\nRetained raw tail\n(none)")
		return b.String()
	}

	b.WriteString("\n\nRetained raw tail\n")
	b.WriteString("These rows are marked compaction_tail=true: hidden from normal transcript rendering, preserved in SQLite, and included in active LLM context.\n")
	for i, msg := range tail {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("--- retained message %d/%d: %s ---\n", i+1, len(tail), formatCompactionDebugMessageRef(msg)))
		b.WriteString(strings.TrimRight(compactionDebugMessageText(msg), "\n"))
	}
	return b.String()
}

func formatCompactionDebugMessageRef(msg session.Message) string {
	parts := []string{fmt.Sprintf("role=%s", msg.Role)}
	if msg.Sequence >= 0 {
		parts = append(parts, fmt.Sprintf("seq=%d", msg.Sequence))
	}
	if msg.ID > 0 {
		parts = append(parts, fmt.Sprintf("id=%d", msg.ID))
	}
	if msg.CompactionTail {
		parts = append(parts, "compaction_tail=true")
	}
	return strings.Join(parts, ", ")
}

func compactionDebugMessageText(msg session.Message) string {
	var sections []string
	for _, part := range msg.Parts {
		switch part.Type {
		case llm.PartText, llm.PartFile:
			if part.Text != "" {
				sections = append(sections, part.Text)
			}
			if reasoning := compactionDebugReasoningText(part); reasoning != "" {
				sections = append(sections, "[Reasoning]\n"+reasoning)
			}
		case llm.PartImage:
			sections = append(sections, compactionDebugImageText(part))
		case llm.PartToolCall:
			if part.ToolCall != nil {
				sections = append(sections, compactionDebugToolCallText(part.ToolCall))
			}
		case llm.PartToolResult:
			if part.ToolResult != nil {
				sections = append(sections, compactionDebugToolResultText(part.ToolResult))
			}
		}
	}
	if len(sections) == 0 && strings.TrimSpace(msg.TextContent) != "" {
		sections = append(sections, msg.TextContent)
	}
	if len(sections) == 0 {
		return "(empty)"
	}
	return strings.Join(sections, "\n\n")
}

func compactionDebugReasoningText(part llm.Part) string {
	if len(part.ReasoningSummaryParts) > 0 {
		return strings.Join(part.ReasoningSummaryParts, "\n\n")
	}
	return strings.TrimSpace(part.ReasoningContent)
}

func compactionDebugImageText(part llm.Part) string {
	if part.ImageData != nil {
		return fmt.Sprintf("[Image: %s, %d bytes]", part.ImageData.MediaType, len(part.ImageData.Base64))
	}
	if part.ImagePath != "" {
		return "[Image: " + part.ImagePath + "]"
	}
	return "[Image]"
}

func compactionDebugToolCallText(tc *llm.ToolCall) string {
	var b strings.Builder
	b.WriteString("[Tool call]\n")
	b.WriteString("Name: ")
	b.WriteString(tc.Name)
	if tc.ID != "" {
		b.WriteString("\nID: ")
		b.WriteString(tc.ID)
	}
	if tc.ToolInfo != "" {
		b.WriteString("\nInfo: ")
		b.WriteString(tc.ToolInfo)
	}
	b.WriteString("\nArguments:\n")
	b.WriteString(formatCompactionDebugJSON(tc.Arguments))
	return b.String()
}

func compactionDebugToolResultText(tr *llm.ToolResult) string {
	var b strings.Builder
	b.WriteString("[Tool result]\n")
	b.WriteString("Name: ")
	b.WriteString(tr.Name)
	if tr.ID != "" {
		b.WriteString("\nID: ")
		b.WriteString(tr.ID)
	}
	if tr.IsError {
		b.WriteString("\nStatus: error")
	} else {
		b.WriteString("\nStatus: ok")
	}
	b.WriteString("\nContent:\n")
	if tr.Content != "" {
		b.WriteString(tr.Content)
	} else {
		b.WriteString("(empty)")
	}
	return b.String()
}

func formatCompactionDebugJSON(data json.RawMessage) string {
	if len(data) == 0 {
		return "{}"
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return string(data)
	}
	formatted, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return string(data)
	}
	return string(formatted)
}

func textLineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func visualLineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func isInspectorCompactionSummaryMessage(msg session.Message) bool {
	if llm.IsInternalCompactionSummaryText(msg.TextContent) {
		return true
	}
	for _, part := range msg.Parts {
		if part.Type == llm.PartText && llm.IsInternalCompactionSummaryText(part.Text) {
			return true
		}
	}
	return false
}

// renderMessage renders a single message with its parts (backward compat, no item tracking)
func (r *ContentRenderer) renderMessage(msg session.Message) string {
	content, _, _ := r.renderMessageWithItems(msg, "", 0)
	return content
}

// renderMessageWithItems renders a single message and tracks content items for truncation.
// Returns the rendered content, content items, and the number of lines rendered.
func (r *ContentRenderer) renderMessageWithItems(msg session.Message, msgID string, startLine int) (string, []ContentItem, int) {
	theme := r.styles.Theme()
	var b strings.Builder
	var items []ContentItem
	lineCount := 0

	// Role header
	roleStyle := lipgloss.NewStyle().Bold(true)
	switch msg.Role {
	case llm.RoleUser:
		roleStyle = roleStyle.Foreground(theme.Primary)
		content, item := r.renderBoxWithItem("User", roleStyle, r.renderUserContent(msg), msgID, "message", startLine)
		b.WriteString(content)
		if item != nil {
			items = append(items, *item)
			lineCount = item.EndLine - startLine
		}
	case llm.RoleAssistant:
		roleStyle = roleStyle.Foreground(theme.Secondary)
		content, assistantItems := r.renderAssistantContentWithItems(msg, msgID, startLine)
		boxContent, item := r.renderBoxWithItem("Assistant", roleStyle, content, msgID, "message", startLine)
		b.WriteString(boxContent)
		if item != nil {
			items = append(items, *item)
			lineCount = item.EndLine - startLine
		}
		items = append(items, assistantItems...)
	case llm.RoleSystem:
		roleStyle = roleStyle.Foreground(theme.Warning)
		content, item := r.renderBoxWithItem("System", roleStyle, r.renderTextContent(msg), msgID, "message", startLine)
		b.WriteString(content)
		if item != nil {
			items = append(items, *item)
			lineCount = item.EndLine - startLine
		}
	case llm.RoleDeveloper:
		roleStyle = roleStyle.Foreground(theme.Spinner)
		content, item := r.renderBoxWithItem("Developer", roleStyle, r.renderTextContent(msg), msgID, "message", startLine)
		b.WriteString(content)
		if item != nil {
			items = append(items, *item)
			lineCount = item.EndLine - startLine
		}
	case llm.RoleEvent:
		roleStyle = roleStyle.Foreground(theme.Muted)
		content, item := r.renderBoxWithItem("Event", roleStyle, r.renderTextContent(msg), msgID, "message", startLine)
		b.WriteString(content)
		if item != nil {
			items = append(items, *item)
			lineCount = item.EndLine - startLine
		}
	case llm.RoleTool:
		content, toolItems := r.renderToolResultsWithItems(msg, msgID, startLine)
		b.WriteString(content)
		items = append(items, toolItems...)
		// For tool results, compute line count from items or fall back to newline count + 1
		if len(toolItems) > 0 {
			lastItem := toolItems[len(toolItems)-1]
			lineCount = lastItem.EndLine - startLine
		} else {
			lineCount = strings.Count(content, "\n") + 1
		}
	}

	return b.String(), items, lineCount
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
	// Note: no newline after bottom border, but it still occupies a visual line
	lineCount++ // Count the bottom border as a line for accurate line tracking

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
		switch part.Type {
		case llm.PartSkillActivation:
			if part.SkillActivation != nil {
				texts = append(texts, formatSkillActivationProvenance(part.SkillActivation))
			}
		case llm.PartText:
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
	}
	if len(texts) == 0 {
		return "(empty)"
	}
	return strings.Join(texts, "\n\n")
}

func formatSkillActivationProvenance(provenance *llm.SkillActivationProvenance) string {
	if provenance == nil {
		return ""
	}
	var lines []string
	lines = append(lines, "Skill activation: "+provenance.Name)
	appendField := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			lines = append(lines, label+": "+value)
		}
	}
	appendField("Source", provenance.Source)
	appendField("Source path", provenance.SourcePath)
	appendField("Origin", provenance.Origin)
	appendField("Execution", provenance.Execution)
	appendField("Arguments", provenance.RawArguments)
	appendField("Agent", provenance.Agent)
	appendField("Model", provenance.Model)
	appendField("Run ID", provenance.RunID)
	appendField("Child session", provenance.ChildSessionID)
	appendField("Status", provenance.Status)
	appendField("Started", provenance.StartedAt)
	appendField("Completed", provenance.CompletedAt)
	if provenance.AllowedToolsPresent {
		allowed := "(none)"
		if len(provenance.AllowedTools) > 0 {
			allowed = strings.Join(provenance.AllowedTools, ", ")
		}
		appendField("Allowed tools", allowed)
	}
	return strings.Join(lines, "\n")
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
			if reasoning := r.renderReasoningForPart(part); reasoning != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
					currentLine += 2
				}
				b.WriteString(reasoning)
				currentLine += strings.Count(reasoning, "\n")
				hasText = true
			}
			if part.Text != "" {
				if hasText {
					b.WriteString("\n")
					currentLine++
				}
				b.WriteString(part.Text)
				currentLine += strings.Count(part.Text, "\n")
				hasText = true
			}
		case llm.PartImage:
			if part.ImageData != nil {
				if hasText {
					b.WriteString("\n")
					currentLine++
				}
				meta := fmt.Sprintf("[Image: %s, %d bytes]", part.ImageData.MediaType, len(part.ImageData.Base64))
				b.WriteString(meta)
				currentLine += strings.Count(meta, "\n")
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

func reasoningSummaryDisplayContent(part llm.Part) string {
	if len(part.ReasoningSummaryParts) == 0 {
		return part.ReasoningContent
	}
	var parts []string
	for _, text := range part.ReasoningSummaryParts {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	if len(parts) == 0 {
		return part.ReasoningContent
	}
	return strings.Join(parts, "\n\n")
}

func (r *ContentRenderer) renderReasoningForPart(part llm.Part) string {
	hasReasoningContent := part.ReasoningContent != "" || len(part.ReasoningSummaryParts) > 0
	kind := llm.NormalizeStoredReasoningKind(part.ReasoningKind, hasReasoningContent)
	switch kind {
	case llm.ReasoningKindSummary:
		return r.renderReasoningSummary(part)
	case llm.ReasoningKindRaw, llm.ReasoningKindUnknown:
		return r.renderProviderReasoning(part)
	default:
		return ""
	}
}

func (r *ContentRenderer) renderProviderReasoning(part llm.Part) string {
	content := strings.TrimSpace(part.ReasoningContent)
	if content == "" {
		return ""
	}
	title := strings.TrimSpace(part.ReasoningSummaryTitle)
	if title == "" {
		title = "Thinking..."
	}
	header := title
	if title != "Thinking..." {
		header = "Thought: " + title
	}
	return r.renderReasoningBlock(header, content, true)
}

func (r *ContentRenderer) renderReasoningSummary(part llm.Part) string {
	hasReasoningContent := part.ReasoningContent != "" || len(part.ReasoningSummaryParts) > 0
	if llm.NormalizeStoredReasoningKind(part.ReasoningKind, hasReasoningContent) != llm.ReasoningKindSummary {
		return ""
	}
	content := strings.TrimSpace(reasoningSummaryDisplayContent(part))
	if content == "" {
		return ""
	}
	parsed := internalreasoning.ParseReasoningSummary(content)
	title := strings.TrimSpace(part.ReasoningSummaryTitle)
	if title == "" {
		title = parsed.Title
	}
	body := strings.TrimSpace(parsed.Body)
	if body == "" {
		body = content
	}
	if title == "" {
		title = "Thinking..."
	}
	header := title
	if title != "Thinking..." {
		header = "Thought: " + title
	}
	return r.renderReasoningBlock(header, body, false)
}

func (r *ContentRenderer) renderReasoningBlock(header, body string, raw bool) string {
	innerWidth := max(r.width-4, 20)
	theme := r.styles.Theme()
	headerStyle := lipgloss.NewStyle().Foreground(theme.ReasoningHeader).Italic(true)
	bodyColor := theme.ReasoningSummary
	if raw {
		bodyColor = theme.ReasoningRaw
	}
	bodyStyle := lipgloss.NewStyle().Foreground(bodyColor).Italic(true)
	var b strings.Builder
	for i, line := range wrapLine(header, innerWidth) {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(headerStyle.Render(line))
	}
	if strings.TrimSpace(body) != "" {
		for _, line := range strings.Split(body, "\n") {
			for _, wrapped := range wrapLine(line, innerWidth) {
				b.WriteString("\n")
				b.WriteString(bodyStyle.Render(wrapped))
			}
		}
	}
	return b.String()
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
		case llm.RoleDeveloper:
			roleLabel = "Developer"
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
			case llm.PartImage:
				if part.ImageData != nil {
					contentText += fmt.Sprintf("[Image: %s]", part.ImageData.MediaType)
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
