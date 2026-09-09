package llm

import (
	"fmt"
	"strings"
	"time"
)

// ConversationStartMessage creates the developer context used to ground a new
// conversation in time. The value is deliberately a fixed start time, not a
// claim about the current time on later turns.
func ConversationStartMessage(start time.Time) Message {
	_, offsetSeconds := start.Zone()
	offsetSign := "+"
	if offsetSeconds < 0 {
		offsetSign = "-"
		offsetSeconds = -offsetSeconds
	}
	offset := fmt.Sprintf("%s%02d:%02d", offsetSign, offsetSeconds/3600, offsetSeconds%3600/60)
	text := fmt.Sprintf(
		"Conversation started at %s (UTC%s). This timestamp is fixed and does not update during the conversation.",
		start.Format("2006-01-02 15:04 MST"), offset,
	)
	return Message{Role: RoleDeveloper, Parts: []Part{
		{Type: PartConversationStart},
		{Type: PartText, Text: text},
	}}
}

// IsConversationStartMessage reports whether message carries the durable marker
// for immutable conversation-start context.
func IsConversationStartMessage(message Message) bool {
	if message.Role != RoleDeveloper {
		return false
	}
	hasMarker := false
	hasText := false
	for _, part := range message.Parts {
		switch part.Type {
		case PartConversationStart:
			hasMarker = true
		case PartText:
			hasText = hasText || strings.TrimSpace(part.Text) != ""
		}
	}
	return hasMarker && hasText
}

// ConversationStartFrom returns the earliest valid conversation-start message.
func ConversationStartFrom(messages []Message) (Message, bool) {
	for _, message := range messages {
		if IsConversationStartMessage(message) {
			return message, true
		}
	}
	return Message{}, false
}

// InsertConversationStart carries source's immutable start context into messages.
// Existing destination context wins, and the source message is inserted after
// leading system/developer context so it remains immediately before the first
// conversational turn.
func InsertConversationStart(messages []Message, source []Message) []Message {
	if _, exists := ConversationStartFrom(messages); exists {
		return messages
	}
	start, ok := ConversationStartFrom(source)
	if !ok {
		return messages
	}
	insertAt := 0
	for insertAt < len(messages) && (messages[insertAt].Role == RoleSystem || messages[insertAt].Role == RoleDeveloper) {
		insertAt++
	}
	out := make([]Message, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, start)
	out = append(out, messages[insertAt:]...)
	return out
}

// WithoutConversationStart removes marked start context while preserving all
// other messages. It is used when the destination agent explicitly opts out.
func WithoutConversationStart(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	changed := false
	for _, message := range messages {
		if IsConversationStartMessage(message) {
			changed = true
			continue
		}
		out = append(out, message)
	}
	if !changed {
		return messages
	}
	return out
}

// BeginConversation inserts newly captured start context into a first-turn
// message set. Existing markers win; established assistant/tool history prevents
// insertion so legacy sessions are never mislabeled with their reload time.
func BeginConversation(messages []Message, start time.Time) []Message {
	if _, exists := ConversationStartFrom(messages); exists {
		return messages
	}
	for _, message := range messages {
		switch message.Role {
		case RoleAssistant, RoleTool:
			return messages
		}
	}
	return InsertConversationStart(messages, []Message{ConversationStartMessage(start)})
}
