package inspector

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/term-llm/term-llm/internal/inspect"
	"github.com/term-llm/term-llm/internal/llm/core"
	internalreasoning "github.com/term-llm/term-llm/internal/reasoning"
)

func compactionDebugReasoningText(part core.Part) string {
	display := core.ReasoningForDisplay(part)
	if display == nil {
		return ""
	}
	if len(display.SummaryParts) > 0 {
		return strings.Join(display.SummaryParts, "\n\n")
	}
	return strings.TrimSpace(display.Content)
}

func reasoningSummaryDisplayContent(part core.Part) string {
	return inspect.ReasoningSummaryDisplayContent(part)
}

func (r *ContentRenderer) renderReasoningForPart(part core.Part) string {
	display := core.ReasoningForDisplay(part)
	if display == nil {
		return ""
	}
	part.ReasoningContent = display.Content
	part.ReasoningSummaryParts = display.SummaryParts
	part.ReasoningKind = core.NormalizeStoredReasoningKind(display.Kind, display.Content != "" || len(display.SummaryParts) > 0)
	part.ReasoningSummaryTitle = display.SummaryTitle
	switch part.ReasoningKind {
	case core.ReasoningKindSummary:
		return r.renderReasoningSummary(part)
	case core.ReasoningKindRaw, core.ReasoningKindUnknown:
		return r.renderProviderReasoning(part)
	default:
		return ""
	}
}

func (r *ContentRenderer) renderProviderReasoning(part core.Part) string {
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

func (r *ContentRenderer) renderReasoningSummary(part core.Part) string {
	hasReasoningContent := part.ReasoningContent != "" || len(part.ReasoningSummaryParts) > 0
	if core.NormalizeStoredReasoningKind(part.ReasoningKind, hasReasoningContent) != core.ReasoningKindSummary {
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
