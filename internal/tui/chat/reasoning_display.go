package chat

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

// reasoningDisplayPart projects a read-only display selection into the legacy
// fields consumed by the shared renderer. Replay metadata on stored parts is
// never modified or passed through as visible reasoning.
func reasoningDisplayPart(part core.Part) core.Part {
	display := core.ReasoningForDisplay(part)
	part.ReasoningContent = ""
	part.ReasoningSummaryParts = nil
	part.ReasoningKind = ""
	part.ReasoningSummaryTitle = ""
	part.ReasoningDisplay = nil
	part.ReasoningReplayOnly = false
	if display != nil {
		part.ReasoningContent = display.Content
		part.ReasoningSummaryParts = display.SummaryParts
		part.ReasoningKind = core.NormalizeStoredReasoningKind(display.Kind, display.Content != "" || len(display.SummaryParts) > 0)
		part.ReasoningSummaryTitle = display.SummaryTitle
	}
	return part
}

// reasoningDisplayMessages only copies messages with ordered metadata. Legacy
// history retains its existing rendering/cache path and all parts retain order.
func reasoningDisplayMessages(messages []session.Message) []session.Message {
	var projected []session.Message
	for i, msg := range messages {
		var parts []core.Part
		for j, part := range msg.Parts {
			if part.ReasoningDisplay == nil && !part.ReasoningReplayOnly {
				continue
			}
			if parts == nil {
				parts = append([]core.Part(nil), msg.Parts...)
			}
			parts[j] = reasoningDisplayPart(part)
		}
		if parts != nil {
			if projected == nil {
				projected = append([]session.Message(nil), messages...)
			}
			projected[i].Parts = parts
		}
	}
	if projected != nil {
		return projected
	}
	return messages
}
