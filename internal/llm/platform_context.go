package llm

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// PlatformContextMessage creates a marked developer message describing the
// active interaction surface. The marker lets compaction retain only the latest
// mode context while provider projection sends only its text.
func PlatformContextMessage(text string) core.Message {
	return PlatformContextMessageForMode(text, "")
}

// PlatformContextMessageForMode records an optional interaction mode alongside
// the context. The mode is persistence metadata, not provider-facing text.
func PlatformContextMessageForMode(text, mode string) core.Message {
	text = strings.TrimSpace(text)
	if text == "" {
		return core.Message{}
	}
	return core.Message{Role: core.RoleDeveloper, Parts: []core.Part{
		{Type: core.PartPlatformContext, Text: mode},
		{Type: core.PartText, Text: text},
	}}
}

// PlatformContextMode returns the explicit mode marker without parsing prompts.
func PlatformContextMode(message core.Message) string {
	if !IsPlatformContextMessage(message) {
		return ""
	}
	for _, part := range message.Parts {
		if part.Type == core.PartPlatformContext {
			return part.Text
		}
	}
	return ""
}

// IsPlatformContextMessage reports whether message is marked platform context.
func IsPlatformContextMessage(message core.Message) bool {
	if message.Role != core.RoleDeveloper {
		return false
	}
	hasMarker := false
	hasText := false
	for _, part := range message.Parts {
		switch part.Type {
		case core.PartPlatformContext:
			hasMarker = true
		case core.PartText:
			hasText = hasText || strings.TrimSpace(part.Text) != ""
		}
	}
	return hasMarker && hasText
}

// PlatformContextFrom returns the latest effective platform context.
func PlatformContextFrom(messages []core.Message) (core.Message, bool) {
	for index := len(messages) - 1; index >= 0; index-- {
		if IsPlatformContextMessage(messages[index]) {
			return messages[index], true
		}
	}
	return core.Message{}, false
}

// InsertPlatformContext carries the latest marked platform context into a
// reconstructed history unless the destination already contains one.
func InsertPlatformContext(messages []core.Message, source []core.Message) []core.Message {
	if _, exists := PlatformContextFrom(messages); exists {
		return messages
	}
	platform, ok := PlatformContextFrom(source)
	if !ok {
		return messages
	}
	insertAt := 0
	for insertAt < len(messages) && messages[insertAt].Role == core.RoleSystem {
		insertAt++
	}
	out := make([]core.Message, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, platform)
	out = append(out, messages[insertAt:]...)
	return out
}
