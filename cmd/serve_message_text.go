package cmd

import (
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// appendSessionMessageText projects text without losing attachment chips when a
// persisted display override hides provider-only context.
func appendSessionMessageText(entry *sessionMessageEntry, msg *session.Message, p llm.Part, embeddedFiles map[string]bool, displayText string) {
	text := p.Text
	if msg.Role == llm.RoleUser {
		for _, name := range llm.ExtractEmbeddedFileNames(text) {
			if embeddedFiles[name] {
				continue
			}
			embeddedFiles[name] = true
			entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "file", Text: name})
		}
		text = llm.StripEmbeddedFileText(text)
	}
	// A text override must not hide attachment chips discovered above.
	if displayText != "" {
		text = ""
	}
	if text != "" {
		entry.Parts = append(entry.Parts, sessionMessagePartEntry{
			Type:      "text",
			Text:      text,
			CreatedAt: p.CreatedAt,
		})
	}
}
