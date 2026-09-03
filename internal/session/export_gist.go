package session

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/agents/gist"
)

// ShareFiles builds the canonical HTML and Markdown transcript bundle.
func ShareFiles(sess *Session, messages []Message, opts ExportOptions) (map[string]string, error) {
	markdown := ExportToMarkdown(sess, messages, opts)
	html, err := ExportToHTML(sess, messages, opts)
	if err != nil {
		return nil, fmt.Errorf("render HTML transcript: %w", err)
	}
	return map[string]string{"index.html": html, "session.md": markdown}, nil
}

// GistFiles is retained for the explicit GitHub-only export commands.
func GistFiles(sess *Session, messages []Message, opts ExportOptions) (map[string]string, error) {
	return ShareFiles(sess, messages, opts)
}

// GistPreviewURL returns the gisthost preview URL for a valid gist ID.
func GistPreviewURL(id string) string {
	return gist.PreviewURL(id)
}
