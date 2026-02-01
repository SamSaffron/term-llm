package ui

import (
	"regexp"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
)

// multiNewlineRe matches 3 or more consecutive newlines.
var multiNewlineRe = regexp.MustCompile(`\n{3,}`)

// NormalizeNewlines reduces 3+ consecutive newlines to 2 (one blank line max).
// This fixes inconsistent spacing between headers caused by glamour's hardcoded
// extra newline before headings combined with our BlockSuffix settings.
func NormalizeNewlines(s string) string {
	return multiNewlineRe.ReplaceAllString(s, "\n\n")
}

// rendererCache provides width-keyed caching of glamour renderers.
// Creating a renderer is expensive; caching by width avoids recreation.
var rendererCache sync.Map // map[int]*glamour.TermRenderer

// getRenderer returns a cached renderer for the given width, creating one if needed.
func getRenderer(width int) (*glamour.TermRenderer, error) {
	if cached, ok := rendererCache.Load(width); ok {
		return cached.(*glamour.TermRenderer), nil
	}

	style := GlamourStyle()
	margin := uint(0)
	style.Document.Margin = &margin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = &margin

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width-1), // slight margin to avoid trailing spaces at terminal edge
	)
	if err != nil {
		return nil, err
	}

	// Store for future use (race-safe: if another goroutine stored first, we just discard ours)
	rendererCache.Store(width, renderer)
	return renderer, nil
}

// RenderMarkdown renders markdown content using glamour with standard styling.
// This is the main function for rendering markdown in streaming contexts.
// On error, returns the original content unchanged.
func RenderMarkdown(content string, width int) string {
	if content == "" {
		return ""
	}

	rendered, err := RenderMarkdownWithError(content, width)
	if err != nil {
		return content
	}
	return rendered
}

// RenderMarkdownWithError renders markdown content and returns any errors.
// Use this variant when error handling is needed.
func RenderMarkdownWithError(content string, width int) (string, error) {
	renderer, err := getRenderer(width)
	if err != nil {
		return "", err
	}

	// Normalize tabs to 2 spaces before rendering to prevent glamour
	// from expanding them to 8 spaces (its default), which causes
	// code blocks to overflow terminal width
	content = strings.ReplaceAll(content, "\t", "  ")

	rendered, err := renderer.Render(content)
	if err != nil {
		return "", err
	}

	normalized := NormalizeNewlines(rendered)
	return strings.TrimSpace(normalized), nil
}
