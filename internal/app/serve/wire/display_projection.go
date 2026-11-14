package wire

import (
	"strings"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/reasoning"
)

// ReasoningPartDisplay projects ordered or legacy reasoning with the same
// summary-part precedence and display limits used by web history.
func ReasoningPartDisplay(part core.Part, role core.Role, cfg config.ReasoningConfig) (kind, title, body string) {
	display := core.ReasoningForDisplay(part)
	if display == nil || role != core.RoleAssistant || !reasoning.InteractiveSourceAllows(string(display.Kind), cfg) || (display.Kind == core.ReasoningKindSummary && !cfg.PersistSummaries) {
		return "", "", ""
	}
	kind = string(display.Kind)
	content := display.Content
	var parts []string
	for _, part := range display.SummaryParts {
		if text := strings.TrimSpace(part); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) > 0 {
		content = strings.Join(parts, "\n\n")
	}
	title, body = ReasoningDisplay(kind, content, strings.TrimSpace(display.SummaryTitle), cfg)
	if strings.TrimSpace(body) == "" {
		body = ""
	}
	return kind, title, body
}
