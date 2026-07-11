package session

import (
	"fmt"
	"regexp"
)

var gistIDPattern = regexp.MustCompile(`^[a-f0-9]+$`)

// GistFiles builds the canonical HTML and Markdown transcript files.
func GistFiles(sess *Session, messages []Message, opts ExportOptions) (map[string]string, error) {
	markdown := ExportToMarkdown(sess, messages, opts)
	html, err := ExportToHTML(sess, messages, opts)
	if err != nil {
		return nil, fmt.Errorf("render HTML transcript: %w", err)
	}
	return map[string]string{"index.html": html, "session.md": markdown}, nil
}

// GistPreviewURL returns the gisthost preview URL for a valid gist ID.
func GistPreviewURL(id string) string {
	if !gistIDPattern.MatchString(id) {
		return ""
	}
	return "https://gisthost.github.io/?" + id + "/index.html"
}
