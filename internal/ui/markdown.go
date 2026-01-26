package ui

import (
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
)

// Package-level renderer cache to avoid expensive recreation during streaming
var (
	mdRendererCache struct {
		sync.Mutex
		renderer *glamour.TermRenderer
		width    int
	}
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
	mdRendererCache.Lock()
	defer mdRendererCache.Unlock()

	// Reuse cached renderer if width matches
	if mdRendererCache.renderer != nil && mdRendererCache.width == width {
		rendered, err := mdRendererCache.renderer.Render(content)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(rendered), nil
	}

	// Create new renderer and cache it
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

	mdRendererCache.renderer = renderer
	mdRendererCache.width = width

	rendered, err := renderer.Render(content)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(rendered), nil
}
