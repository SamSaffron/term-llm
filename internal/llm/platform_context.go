package llm

import "strings"

// PlatformContextMessage creates a marked developer message describing the
// active interaction surface. The marker lets compaction retain only the latest
// mode context while provider projection sends only its text.
func PlatformContextMessage(text string) Message {
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}
	}
	return Message{Role: RoleDeveloper, Parts: []Part{
		{Type: PartPlatformContext},
		{Type: PartText, Text: text},
	}}
}

// IsPlatformContextMessage reports whether message is marked platform context.
func IsPlatformContextMessage(message Message) bool {
	if message.Role != RoleDeveloper {
		return false
	}
	hasMarker := false
	hasText := false
	for _, part := range message.Parts {
		switch part.Type {
		case PartPlatformContext:
			hasMarker = true
		case PartText:
			hasText = hasText || strings.TrimSpace(part.Text) != ""
		}
	}
	return hasMarker && hasText
}

// PlatformContextFrom returns the latest effective platform context.
func PlatformContextFrom(messages []Message) (Message, bool) {
	for index := len(messages) - 1; index >= 0; index-- {
		if IsPlatformContextMessage(messages[index]) {
			return messages[index], true
		}
	}
	return Message{}, false
}

// InsertPlatformContext carries the latest marked platform context into a
// reconstructed history unless the destination already contains one.
func InsertPlatformContext(messages []Message, source []Message) []Message {
	if _, exists := PlatformContextFrom(messages); exists {
		return messages
	}
	platform, ok := PlatformContextFrom(source)
	if !ok {
		return messages
	}
	insertAt := 0
	for insertAt < len(messages) && messages[insertAt].Role == RoleSystem {
		insertAt++
	}
	out := make([]Message, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, platform)
	out = append(out, messages[insertAt:]...)
	return out
}
