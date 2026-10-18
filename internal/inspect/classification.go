package inspect

import (
	"encoding/json"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
)

// CompactionBoundaryIndex mirrors the CLI's persisted sequence lookup and optional
// in-memory fallback, including skipping system rows at the boundary.
func CompactionBoundaryIndex(messages []session.Message, hasBoundary bool, sequence, fallback int) int {
	if !hasBoundary {
		return -1
	}
	idx := -1
	if sequence >= 0 {
		for i, msg := range messages {
			if msg.Sequence >= sequence {
				idx = i
				break
			}
		}
	}
	if idx < 0 && fallback >= 0 && fallback < len(messages) {
		idx = fallback
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

// StoredReasoningKind applies the same legacy summary classification as the CLI.
func StoredReasoningKind(part llm.Part) llm.ReasoningKind {
	return llm.NormalizeStoredReasoningKind(part.ReasoningKind, part.ReasoningContent != "" || len(part.ReasoningSummaryParts) > 0)
}

// ReasoningDisplay returns display-safe content, never encrypted replay material.
func ReasoningDisplay(part llm.Part) (kind llm.ReasoningKind, title, body string) {
	kind = StoredReasoningKind(part)
	content := strings.TrimSpace(part.ReasoningContent)
	if kind == llm.ReasoningKindEncrypted || (strings.TrimSpace(ReasoningSummaryDisplayContent(part)) == "" && part.ReasoningEncryptedContent != "") {
		return llm.ReasoningKindEncrypted, "Encrypted reasoning", ""
	}
	title = strings.TrimSpace(part.ReasoningSummaryTitle)
	body = content
	if kind == llm.ReasoningKindSummary {
		content = strings.TrimSpace(ReasoningSummaryDisplayContent(part))
		parsed := reasoning.ParseReasoningSummary(content)
		if title == "" {
			title = parsed.Title
		}
		body = strings.TrimSpace(parsed.Body)
		if body == "" {
			body = content
		}
	}
	if title == "" {
		title = "Thinking..."
	}
	return kind, title, body
}

// FormatJSON pretty-prints valid JSON and preserves malformed legacy arguments.
func FormatJSON(data json.RawMessage) string {
	if len(data) == 0 {
		return "{}"
	}
	var parsed any
	if json.Unmarshal(data, &parsed) != nil {
		return string(data)
	}
	formatted, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return string(data)
	}
	return string(formatted)
}
