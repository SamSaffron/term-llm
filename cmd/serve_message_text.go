package cmd

import (
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// sessionMessageFilePart projects a structured upload as a file chip. Unlike
// provider fallback text, metadata does not expose file bodies or server paths.
// Files do not have a public download route, so use the existing name-only chip.
func sessionMessageFilePart(part llm.Part) sessionMessagePartEntry {
	entry := sessionMessagePartEntry{Type: "file", Text: "upload", MimeType: "application/octet-stream"}
	if part.FileData != nil {
		entry.Text = llm.EmbeddedFileDisplayName(part.FileData.Filename)
		if part.FileData.MediaType != "" {
			entry.MimeType = part.FileData.MediaType
		}
	}
	return entry
}

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
