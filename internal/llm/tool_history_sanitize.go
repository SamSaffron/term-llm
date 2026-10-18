package llm

import "github.com/samsaffron/term-llm/internal/llm/core"

// FilterConversationMessages removes durable UI/session event markers and other
// non-conversation rows before context is sent to providers, compaction, handover,
// or token estimation. It returns a shallow copy preserving cache anchors.
func FilterConversationMessages(messages []Message) []Message {
	return core.FilterConversationMessages(messages)
}

// sanitizeToolHistory removes dangling tool calls and orphan tool results.
func sanitizeToolHistory(messages []Message) []Message { return core.SanitizeToolHistory(messages) }

func cloneParts(parts []Part) []Part { return core.CloneParts(parts) }

func clonePart(part Part) (Part, bool) { return core.ClonePart(part) }

func cloneToolContentParts(parts []ToolContentPart) []ToolContentPart {
	return core.CloneToolContentParts(parts)
}
