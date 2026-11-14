// Package inspect projects stored session history into a read-only, presentation-neutral
// inspector. Provider replay state and encoded attachments are never projected.
package inspect

import (
	"encoding/json"
	"strings"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/tools"
)

// CompactionTailEnd returns the first row after the summary's contiguous retained tail.
func CompactionTailEnd(messages []session.Message, summaryIdx int) int {
	end := summaryIdx + 1
	for end < len(messages) && messages[end].CompactionTail {
		end++
	}
	return end
}

// IsCompactionSummary detects both structured and legacy stored summary rows.
func IsCompactionSummary(msg session.Message) bool {
	if engine.IsInternalCompactionSummaryText(msg.TextContent) {
		return true
	}
	for _, part := range msg.Parts {
		if part.Type == core.PartText && engine.IsInternalCompactionSummaryText(part.Text) {
			return true
		}
	}
	return false
}

// FormatSkillActivationProvenance formats the same audit text as the CLI inspector.
func FormatSkillActivationProvenance(provenance *core.SkillActivationProvenance) string {
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

// ReasoningSummaryDisplayContent joins selected summary parts, preferring them to text.
// Replay-only aggregates have no display content.
func ReasoningSummaryDisplayContent(part core.Part) string {
	display := core.ReasoningForDisplay(part)
	if display == nil {
		return ""
	}
	if len(display.SummaryParts) == 0 {
		return display.Content
	}
	var parts []string
	for _, text := range display.SummaryParts {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	if len(parts) == 0 {
		return display.Content
	}
	return strings.Join(parts, "\n\n")
}

// encryptedReasoningBytes counts replay bytes independently of the display kind:
// a readable burst may accompany encrypted aggregate metadata on the same part.
func encryptedReasoningBytes(part core.Part) int {
	n := len(part.ReasoningEncryptedContent)
	kind := core.NormalizeStoredReasoningKind(part.ReasoningKind, part.ReasoningContent != "" || len(part.ReasoningSummaryParts) > 0)
	legacyEncryptedOnly := part.ReasoningDisplay == nil && !part.ReasoningReplayOnly && n > 0 && strings.TrimSpace(ReasoningSummaryDisplayContent(part)) == ""
	if kind == core.ReasoningKindEncrypted || legacyEncryptedOnly {
		n += len(part.ReasoningContent)
	}
	return n
}

// ExtractSessionIDFromSpawnAgentResult returns the CLI's first child candidate.
// Callers must validate it against the parent before emitting a navigable link.
func ExtractSessionIDFromSpawnAgentResult(content string) string {
	if strings.HasPrefix(strings.TrimSpace(content), "[") {
		var results []json.RawMessage
		if err := json.Unmarshal([]byte(content), &results); err != nil || len(results) == 0 {
			return ""
		}
		content = string(results[0])
	}
	result, err := tools.ParseSpawnAgentResult(content)
	if err != nil {
		return ""
	}
	return result.SessionID
}
