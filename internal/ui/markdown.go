package ui

import (
	"fmt"
	"image/color"

	rendermarkdown "github.com/samsaffron/term-llm/internal/render/markdown"
)

// colorHex converts a color.Color to a hex string (#rrggbb) for use with
// downstream renderers (chroma, goldmark) that take hex color strings.
func colorHex(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, a := c.RGBA()
	if a == 0 {
		return ""
	}
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// MarkdownRenderOptions controls legacy caller-specific markdown rendering quirks.
type MarkdownRenderOptions struct {
	WrapOffset int

	NormalizeTabs     bool
	NormalizeNewlines bool

	EnsureTrailingLine bool
}

func defaultMarkdownRenderOptions() MarkdownRenderOptions {
	return MarkdownRenderOptions{
		WrapOffset:        1,
		NormalizeTabs:     true,
		NormalizeNewlines: true,
	}
}

func currentMarkdownPalette() rendermarkdown.Palette {
	theme := GetTheme()
	return rendermarkdown.Palette{
		Primary:   colorHex(theme.Primary),
		Secondary: colorHex(theme.Secondary),
		Success:   colorHex(theme.Success),
		Warning:   colorHex(theme.Warning),
		Muted:     colorHex(theme.Muted),
		Text:      colorHex(theme.Text),
	}
}

// RenderMarkdown renders markdown content with the shared terminal renderer.
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
func RenderMarkdownWithError(content string, width int) (string, error) {
	return RenderMarkdownWithOptionsError(content, width, defaultMarkdownRenderOptions())
}

// RenderMarkdownWithOptions renders markdown with caller-specific compatibility options.
// On error, returns the original content unchanged.
func RenderMarkdownWithOptions(content string, width int, options MarkdownRenderOptions) string {
	if content == "" {
		return ""
	}

	rendered, err := RenderMarkdownWithOptionsError(content, width, options)
	if err != nil {
		return content
	}
	return rendered
}

// RenderMarkdownWithOptionsError renders markdown with caller-specific compatibility options.
func RenderMarkdownWithOptionsError(content string, width int, options MarkdownRenderOptions) (string, error) {
	if content == "" {
		return "", nil
	}

	return rendermarkdown.RenderString(content, rendermarkdown.Config{
		Palette:            currentMarkdownPalette(),
		Width:              width,
		WrapOffset:         options.WrapOffset,
		NormalizeTabs:      options.NormalizeTabs,
		NormalizeNewlines:  options.NormalizeNewlines,
		TrimSpace:          true,
		EnsureTrailingLine: options.EnsureTrailingLine,
	})
}
