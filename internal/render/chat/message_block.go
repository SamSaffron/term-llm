package chat

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

// MessageBlock represents a pre-rendered, cacheable message.
// Once rendered, blocks are immutable and can be reused across frames.
type MessageBlock struct {
	// MessageID is the unique identifier for this message
	MessageID int64

	// Rendered is the complete rendered output for this message
	Rendered string

	// Height is the number of lines in the rendered output
	Height int

	// Width is the terminal width when this block was rendered
	// (used for cache invalidation on resize)
	Width int

	// FirstSegmentType/LastSegmentType describe the first and last rendered
	// stream-equivalent segment in this block. History rendering uses them to
	// preserve the same text/tool/thought spacing after sessions are reloaded.
	FirstSegmentType ui.SegmentType
	LastSegmentType  ui.SegmentType
	HasSegmentTypes  bool

	// ReasoningCount is the number of reasoning/thought headers actually rendered
	// in this block. It is used to keep per-block click expansion ordinals aligned
	// with visible history, rather than provider/session metadata that may be hidden.
	ReasoningCount int

	// ReasoningLineOffsets are zero-based line offsets, relative to Rendered, for
	// each actual reasoning/thought header rendered in this block.
	ReasoningLineOffsets []int
}

// MessageBlockRenderer renders session messages to MessageBlocks.
type MessageBlockRenderer struct {
	width                  int
	markdownRenderer       MarkdownRenderer
	theme                  *ui.Theme
	messages               []session.Message // Full message list for tool result lookup
	currentIndex           int               // Current message index in the list
	toolsExpanded          bool
	imageRenderer          ui.ImageArtifactRenderer
	reasoningConfig        config.ReasoningConfig
	reasoningOrdinalBase   int
	reasoningExpandByIndex map[int]bool
	reasoningRenderedCount int
	reasoningLineOffsets   []int
	firstSegmentType       ui.SegmentType
	lastSegmentType        ui.SegmentType
	hasSegmentTypes        bool
}

// Shared theme instance to avoid allocations
var sharedTheme = ui.DefaultStyles().Theme()

// NewMessageBlockRenderer creates a new renderer for message blocks.
func NewMessageBlockRenderer(width int, mdRenderer MarkdownRenderer, toolsExpanded bool) *MessageBlockRenderer {
	return &MessageBlockRenderer{
		width:            width,
		markdownRenderer: mdRenderer,
		theme:            sharedTheme,
		toolsExpanded:    toolsExpanded,
		reasoningConfig:  config.DefaultReasoningConfig(),
	}
}

// NewMessageBlockRendererWithContext creates a new renderer with message context for tool result lookup.
// This allows rendering diffs for edit_file tool calls by finding the corresponding tool result
// in subsequent messages.
func NewMessageBlockRendererWithContext(width int, mdRenderer MarkdownRenderer, messages []session.Message, index int, toolsExpanded bool) *MessageBlockRenderer {
	return &MessageBlockRenderer{
		width:            width,
		markdownRenderer: mdRenderer,
		theme:            sharedTheme,
		messages:         messages,
		currentIndex:     index,
		toolsExpanded:    toolsExpanded,
		reasoningConfig:  config.DefaultReasoningConfig(),
	}
}

// SetImageRenderer configures the renderer used for generated-image artifacts.
func (r *MessageBlockRenderer) SetImageRenderer(renderer ui.ImageArtifactRenderer) {
	r.imageRenderer = renderer
}

// SetReasoningConfig configures reasoning summary/raw history rendering.
func (r *MessageBlockRenderer) SetReasoningConfig(cfg config.ReasoningConfig) {
	r.reasoningConfig = cfg
}

// SetReasoningExpansionOverrides configures optional per-reasoning-block expansion overrides.
// Keys are zero-based reasoning block ordinals in the full rendered history.
func (r *MessageBlockRenderer) SetReasoningExpansionOverrides(base int, overrides map[int]bool) {
	r.reasoningOrdinalBase = base
	r.reasoningExpandByIndex = overrides
}

// Render converts a session.Message to a MessageBlock.
func (r *MessageBlockRenderer) Render(msg *session.Message) *MessageBlock {
	r.reasoningRenderedCount = 0
	r.reasoningLineOffsets = nil
	r.firstSegmentType = ui.SegmentText
	r.lastSegmentType = ui.SegmentText
	r.hasSegmentTypes = false
	var content string

	switch msg.Role {
	case llm.RoleUser:
		if isInternalCompactionSummarySessionMessage(msg) {
			content = r.renderCompactionSummaryPlaceholder(msg)
		} else {
			content = r.renderUserMessage(msg)
		}
		if strings.TrimSpace(content) != "" {
			r.noteRenderedSegment(ui.SegmentText)
		}
	case llm.RoleAssistant:
		content = r.renderAssistantMessage(msg)
	case llm.RoleEvent:
		content = r.renderEventMessage(msg)
		if strings.TrimSpace(content) != "" {
			r.noteRenderedSegment(ui.SegmentText)
		}
	case llm.RoleSystem:
		// Skip system messages - users can view them via Ctrl+O inspector
		content = ""
	default:
		// Tool messages and other roles - skip (tool results are verbose)
		content = ""
	}

	return &MessageBlock{
		MessageID:            msg.ID,
		Rendered:             content,
		Height:               countLines(content),
		Width:                r.width,
		FirstSegmentType:     r.firstSegmentType,
		LastSegmentType:      r.lastSegmentType,
		HasSegmentTypes:      r.hasSegmentTypes,
		ReasoningCount:       r.reasoningRenderedCount,
		ReasoningLineOffsets: append([]int(nil), r.reasoningLineOffsets...),
	}
}

func (r *MessageBlockRenderer) noteRenderedSegment(segmentType ui.SegmentType) {
	if !r.hasSegmentTypes {
		r.firstSegmentType = segmentType
		r.hasSegmentTypes = true
	}
	r.lastSegmentType = segmentType
}

// renderUserMessage renders a user message with prompt styling.
func (r *MessageBlockRenderer) renderUserMessage(msg *session.Message) string {
	var b strings.Builder
	userMsgBg := r.userMessageBackground()
	displayContent, attachmentMeta := r.userDisplayParts(msg)

	promptStyle := lipgloss.NewStyle().
		Foreground(r.theme.Primary).
		Bold(true).
		Background(userMsgBg)
	userMsgStyle := lipgloss.NewStyle().Background(userMsgBg)
	userMetaStyle := lipgloss.NewStyle().
		Foreground(r.theme.Muted).
		Background(userMsgBg)

	// Wrap content to fit terminal width minus prompt
	promptWidth := 2 // "❯ " is 2 cells
	wrapWidth := r.width - promptWidth
	if wrapWidth < 20 {
		wrapWidth = 20
	}
	wrappedContent := wordwrap.String(displayContent, wrapWidth)

	// Render with prompt on first line, indent continuation lines.
	// Each row is padded while the background style is active so the user
	// message block has a clean rectangular background across the terminal.
	lines := strings.Split(wrappedContent, "\n")
	for i, line := range lines {
		if i == 0 {
			b.WriteString(renderUserMessageLine("❯ ", promptStyle, line, userMsgStyle, r.width))
		} else {
			b.WriteString(renderUserMessageLine("  ", userMsgStyle, line, userMsgStyle, r.width))
		}
		b.WriteString("\n")
	}
	if attachmentMeta != "" {
		b.WriteString(renderUserMessageLine("  ", userMsgStyle, attachmentMeta, userMetaStyle, r.width))
		b.WriteString("\n")
	}
	b.WriteString("\n") // Extra blank line after user messages

	return b.String()
}

func (r *MessageBlockRenderer) renderEventMessage(msg *session.Message) string {
	text := strings.TrimSpace(msg.TextContent)
	if marker, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage()); ok {
		text = marker.DisplayText
	} else if marker, ok := llm.ParseRunErrorMarker(msg.ToLLMMessage()); ok {
		text = marker.Message
	}
	if text == "" {
		text = "↔ Session event"
	}
	style := lipgloss.NewStyle().Foreground(r.theme.Muted).Italic(true)
	return style.Render(text) + "\n\n"
}

func (r *MessageBlockRenderer) renderCompactionSummaryPlaceholder(msg *session.Message) string {
	text := internalCompactionSummaryText(msg)
	displayText := llm.CompactionSummaryDisplayText(text)
	lineCount := 0
	if displayText != "" {
		lineCount = strings.Count(displayText, "\n") + 1
	}
	detail := "internal summary hidden"
	if lineCount > 1 {
		detail = fmt.Sprintf("internal summary hidden, %d lines", lineCount)
	}
	style := lipgloss.NewStyle().Foreground(r.theme.Muted).Italic(true)
	line := "↳ Context compacted · " + detail + " · Ctrl+O to inspect"
	wrapWidth := r.width
	if wrapWidth < 20 {
		wrapWidth = 20
	}
	return style.Render(wordwrap.String(line, wrapWidth)) + "\n\n"
}

func isInternalCompactionSummarySessionMessage(msg *session.Message) bool {
	return llm.IsInternalCompactionSummaryText(internalCompactionSummaryText(msg))
}

func internalCompactionSummaryText(msg *session.Message) string {
	if msg == nil {
		return ""
	}
	if llm.IsInternalCompactionSummaryText(msg.TextContent) {
		return msg.TextContent
	}
	for _, part := range msg.Parts {
		if part.Type == llm.PartText && llm.IsInternalCompactionSummaryText(part.Text) {
			return part.Text
		}
	}
	return msg.TextContent
}

func renderUserMessageLine(prefix string, prefixStyle lipgloss.Style, content string, contentStyle lipgloss.Style, width int) string {
	pad := width - ansi.StringWidth(prefix) - ansi.StringWidth(content)
	if pad < 0 {
		pad = 0
	}

	return prefixStyle.Render(prefix) + contentStyle.Render(content+strings.Repeat(" ", pad))
}

func (r *MessageBlockRenderer) userDisplayParts(msg *session.Message) (string, string) {
	displayContent := msg.TextContent
	if len(msg.Parts) > 0 {
		displayContent = extractTextParts(msg.Parts)
	}

	fileNames := llm.ExtractEmbeddedFileNames(displayContent)
	seenFiles := make(map[string]bool, len(fileNames))
	for _, name := range fileNames {
		seenFiles[name] = true
	}
	for _, part := range msg.Parts {
		if part.Type != llm.PartFile || part.FileData == nil {
			continue
		}
		name := llm.EmbeddedFileDisplayName(part.FileData.Filename)
		if name == "" || seenFiles[name] {
			continue
		}
		seenFiles[name] = true
		fileNames = append(fileNames, name)
	}

	// Remove embedded file bodies from the main user-message display.
	displayContent = llm.StripEmbeddedFileText(displayContent)

	imageCount := 0
	for _, part := range msg.Parts {
		if part.Type == llm.PartImage {
			imageCount++
		}
	}
	fileCount := len(fileNames)

	if strings.TrimSpace(displayContent) == "" {
		switch {
		case imageCount > 0 && fileCount > 0:
			displayContent = "[attachments]"
		case imageCount == 1:
			displayContent = "[image]"
		case imageCount > 1:
			displayContent = fmt.Sprintf("[%d images]", imageCount)
		case fileCount == 1:
			displayContent = "[file]"
		case fileCount > 1:
			displayContent = fmt.Sprintf("[%d files]", fileCount)
		}
	}

	var attachmentNames []string
	switch imageCount {
	case 0:
	case 1:
		attachmentNames = append(attachmentNames, "image 1")
	default:
		attachmentNames = append(attachmentNames, fmt.Sprintf("%d images", imageCount))
	}
	attachmentNames = append(attachmentNames, fileNames...)
	if len(attachmentNames) == 0 {
		return displayContent, ""
	}
	return displayContent, fmt.Sprintf("[with: %s]", strings.Join(attachmentNames, ", "))
}

func extractTextParts(parts []llm.Part) string {
	var texts []string
	for _, part := range parts {
		if (part.Type == llm.PartText || part.Type == llm.PartFile) && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func (r *MessageBlockRenderer) userMessageBackground() color.Color {
	return r.theme.UserMsgBg
}

// findToolResult looks for a tool result matching the given tool call ID in subsequent messages.
// Tool results are typically stored in tool messages following the assistant message.
func (r *MessageBlockRenderer) findToolResult(toolCallID string) *llm.ToolResult {
	if r.messages == nil || toolCallID == "" {
		return nil
	}

	// Look through subsequent messages (tool results follow the assistant message)
	for i := r.currentIndex + 1; i < len(r.messages); i++ {
		msg := &r.messages[i]

		// Stop looking if we hit another assistant message
		if msg.Role == llm.RoleAssistant {
			break
		}

		for _, part := range msg.Parts {
			if part.Type == llm.PartToolResult && part.ToolResult != nil {
				if part.ToolResult.ID == toolCallID {
					return part.ToolResult
				}
			}
		}

		// If we hit a user message that has no tool results, we've likely passed the tool turn
		if msg.Role == llm.RoleUser && len(msg.Parts) > 0 {
			hasToolResult := false
			for _, part := range msg.Parts {
				if part.Type == llm.PartToolResult {
					hasToolResult = true
					break
				}
			}
			if !hasToolResult {
				break
			}
		}
	}
	return nil
}

// renderAssistantMessage renders an assistant message with all parts.
func (r *MessageBlockRenderer) renderAssistantMessage(msg *session.Message) string {
	var b strings.Builder
	hasContent := false

	for _, part := range msg.Parts {
		switch part.Type {
		case llm.PartText:
			globalOrdinal := r.reasoningOrdinalBase + r.reasoningRenderedCount
			if reasoningRendered := r.renderReasoningForPart(part, globalOrdinal); reasoningRendered != "" {
				r.reasoningLineOffsets = append(r.reasoningLineOffsets, reasoningAppendHeaderLineOffset(b.String()))
				writeWithBlankLineBefore(&b, reasoningRendered)
				r.noteRenderedSegment(ui.SegmentReasoning)
				hasContent = true
				r.reasoningRenderedCount++
			}
			if part.Text != "" {
				rendered := r.renderMarkdown(part.Text)
				b.WriteString(rendered)
				b.WriteString("\n\n")
				r.noteRenderedSegment(ui.SegmentText)
				hasContent = true
			}
		case llm.PartImage:
			b.WriteString("[Image]\n\n")
			r.noteRenderedSegment(ui.SegmentImage)
			hasContent = true
		case llm.PartToolCall:
			if part.ToolCall != nil {
				result := r.findToolResult(part.ToolCall.ID)
				status := ui.ToolSuccess
				if result != nil && result.IsError {
					status = ui.ToolError
				}
				b.WriteString(ui.RenderToolCallFromPartWithStatus(part.ToolCall, r.width, r.toolsExpanded, status))
				b.WriteString("\n")
				r.noteRenderedSegment(ui.SegmentTool)
				hasContent = true

				if result != nil && len(result.Images) > 0 {
					b.WriteString(r.renderToolImages(result.Images))
					r.noteRenderedSegment(ui.SegmentImage)
				}

				// Render diffs for supported tool calls by looking up the tool result
				switch part.ToolCall.Name {
				case "edit_file", "unified_diff", "spawn_agent", "write_file":
					if result != nil {
						// Prefer structured Diffs (new sessions)
						diffs := result.Diffs
						if len(diffs) == 0 {
							// Fall back to parsing markers from Display/Content (old sessions
							// saved before structured Diffs were added). This fallback can be
							// removed once no saved sessions contain __DIFF__ text markers.
							content := result.Display
							if content == "" {
								content = result.Content
							}
							if part.ToolCall.Name == "spawn_agent" {
								var res struct {
									Output string `json:"output"`
								}
								if err := json.Unmarshal([]byte(content), &res); err == nil {
									content = res.Output
								}
							}
							diffs = ui.ParseDiffMarkers(content)
						}

						for _, diff := range diffs {
							rendered := ui.RenderDiffSegmentWithOperation(diff.File, diff.Old, diff.New, r.width, diff.Line, diff.Operation)
							if rendered != "" {
								b.WriteString(rendered)
								b.WriteString("\n")
								r.noteRenderedSegment(ui.SegmentDiff)
							}
						}
					}
				}
			}
			// Skip PartToolResult - they're in user messages and verbose
		}
	}

	// Fallback: if no parts rendered, use TextContent (for backward compatibility)
	if !hasContent && msg.TextContent != "" {
		rendered := r.renderMarkdown(msg.TextContent)
		b.WriteString(rendered)
		b.WriteString("\n\n")
		r.noteRenderedSegment(ui.SegmentText)
	}

	// Keep tool-only assistant blocks compact: they already include line breaks.
	// Text parts append paragraph spacing above.

	return b.String()
}

// reasoningAppendHeaderLineOffset returns the pre-wrap newline offset for a
// reasoning header about to be appended to current. ANSI escape sequences do
// not contain newlines, so byte-level newline counting is intentional here.
func reasoningAppendHeaderLineOffset(current string) int {
	if strings.TrimSpace(current) == "" {
		return countLines(current)
	}
	switch {
	case strings.HasSuffix(current, "\n\n"):
		return countLines(current)
	case strings.HasSuffix(current, "\n"):
		return countLines(current) + 1
	default:
		return countLines(current) + 2
	}
}

func writeWithBlankLineBefore(b *strings.Builder, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	if b.Len() > 0 {
		current := b.String()
		switch {
		case strings.HasSuffix(current, "\n\n"):
			// already separated
		case strings.HasSuffix(current, "\n"):
			b.WriteString("\n")
		default:
			b.WriteString("\n\n")
		}
	}
	b.WriteString(strings.TrimLeft(content, "\n"))
}

func (r *MessageBlockRenderer) renderToolImages(images []string) string {
	var b strings.Builder
	for _, imagePath := range images {
		if rendered := ui.RenderImageArtifactWithRenderer(imagePath, r.imageRenderer); rendered != "" {
			b.WriteString(rendered)
			if !strings.HasSuffix(rendered, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func reasoningDisplayContent(part llm.Part) string {
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

func (r *MessageBlockRenderer) renderReasoningForPart(part llm.Part, ordinal int) string {
	hasReasoningContent := part.ReasoningContent != "" || len(part.ReasoningSummaryParts) > 0
	kind := llm.NormalizeStoredReasoningKind(part.ReasoningKind, hasReasoningContent)
	if !hasReasoningContent || kind == llm.ReasoningKindEncrypted || kind == llm.ReasoningKindUnknown {
		return ""
	}
	cfg := r.reasoningConfig
	if override, ok := r.reasoningExpandByIndex[ordinal]; ok {
		if override {
			cfg.Display = config.ReasoningDisplayExpanded
		} else {
			cfg.Display = config.ReasoningDisplayCollapsed
		}
	}
	if !internalreasoning.HistoryVisible(string(kind), cfg) {
		return ""
	}
	content := reasoningDisplayContent(part)
	content = internalreasoning.LimitReasoningText(string(kind), content, cfg)
	if strings.TrimSpace(content) == "" {
		return ""
	}
	expanded := internalreasoning.HistoryExpanded(string(kind), cfg)
	title := strings.TrimSpace(part.ReasoningSummaryTitle)
	if title == "" && kind == llm.ReasoningKindSummary {
		title = internalreasoning.SummaryTitle(content, cfg)
	}
	label := strings.TrimSpace(cfg.HiddenLabel)
	if label == "" {
		label = config.DefaultReasoningConfig().HiddenLabel
	}
	if title == "" {
		title = label
	}

	headerStyle := lipgloss.NewStyle().Foreground(r.theme.ReasoningHeader).Italic(true)
	bodyStyle := lipgloss.NewStyle().Foreground(r.theme.ReasoningSummary).Italic(true)
	if kind == llm.ReasoningKindRaw {
		bodyStyle = lipgloss.NewStyle().Foreground(r.theme.ReasoningRaw).Italic(true)
	}
	arrow := "▸"
	if expanded {
		arrow = "▾"
	}
	headerText := fmt.Sprintf("%s %s", arrow, title)
	if title != label {
		headerText = fmt.Sprintf("%s Thought: %s", arrow, title)
	}

	var b strings.Builder
	b.WriteString(headerStyle.Render(headerText))
	b.WriteString("\n")
	if expanded {
		body := content
		if kind == llm.ReasoningKindSummary {
			body = internalreasoning.SummaryBody(content, cfg)
		}
		body = strings.TrimRight(body, "\n")
		if strings.TrimSpace(body) != "" {
			// Separate the affordance/header from the expanded body. Without this,
			// multiple Responses API reasoning summary blocks visually run together.
			b.WriteString("\n")
			rendered := ui.StripANSI(r.renderMarkdown(body))
			for _, line := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
				b.WriteString(bodyStyle.Render(line))
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("\n")
	return b.String()
}

// renderMarkdown renders markdown content using the configured renderer.
func (r *MessageBlockRenderer) renderMarkdown(content string) string {
	if content == "" {
		return ""
	}
	if r.markdownRenderer == nil {
		return content
	}
	return r.markdownRenderer(content, r.width)
}

// countLines counts the number of newlines in a string.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	count := 0
	for _, c := range s {
		if c == '\n' {
			count++
		}
	}
	// Account for final line without trailing newline
	if len(s) > 0 && s[len(s)-1] != '\n' {
		count++
	}
	return count
}

// RenderEmptyHistory renders the "no messages" placeholder.
func RenderEmptyHistory(theme *ui.Theme) string {
	return lipgloss.NewStyle().
		Foreground(theme.Muted).
		Render("No messages yet. Type your question and press Enter.\n\n")
}

// RenderScrollIndicator renders the scroll position indicator.
func RenderScrollIndicator(offset int, theme *ui.Theme) string {
	if offset == 0 {
		return ""
	}
	scrollInfo := "↑ Scrolled up " + strconv.Itoa(offset) + " message(s) · Press G to go to bottom"
	return lipgloss.NewStyle().
		Foreground(theme.Warning).
		Render(scrollInfo) + "\n\n"
}
