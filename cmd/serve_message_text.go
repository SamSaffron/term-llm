package cmd

import (
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// sessionMessageFilePart projects a structured upload as a downloadable file
// chip. Unlike provider fallback text, metadata never exposes file bodies, and
// the download URL carries only the random upload basename. An upload whose
// file was pruned, or that never landed in the uploads dir, keeps its name and
// MIME type but omits file_url so the chip degrades to a non-clickable name.
func (s *serveServer) sessionMessageFilePart(part llm.Part) sessionMessagePartEntry {
	entry := sessionMessagePartEntry{Type: "file", Text: "upload", MimeType: "application/octet-stream", Kind: filePartKindUpload}
	if part.FileData != nil {
		entry.Text = llm.EmbeddedFileDisplayName(part.FileData.Filename)
		if part.FileData.MediaType != "" {
			entry.MimeType = part.FileData.MediaType
		}
		entry.SizeBytes = part.FileData.SizeBytes
	}
	entry.FileURL = s.uploadFileURL(part.FilePath)
	return entry
}

// appendSessionMessageText projects text without losing attachment chips when a
// persisted display override hides provider-only context. Names recovered from
// embedded file markers are references, not uploads: their contents travel in
// the prompt, so they carry no download URL and are marked as such.
func appendSessionMessageText(entry *sessionMessageEntry, msg *session.Message, p llm.Part, embeddedFiles map[string]bool, displayText string) {
	text := p.Text
	if msg.Role == llm.RoleUser {
		for _, name := range llm.ExtractEmbeddedFileNames(text) {
			if embeddedFiles[name] {
				continue
			}
			embeddedFiles[name] = true
			entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "file", Text: name, Kind: filePartKindReference})
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
