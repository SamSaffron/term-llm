package ui

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

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
	style := GlamourStyle()
	margin := uint(0)
	style.Document.Margin = &margin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = &margin

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return "", err
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(rendered), nil
}
