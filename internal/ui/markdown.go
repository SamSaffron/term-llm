package ui

import rendermarkdown "github.com/samsaffron/term-llm/internal/render/markdown"

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
		Primary:   string(theme.Primary),
		Secondary: string(theme.Secondary),
		Success:   string(theme.Success),
		Warning:   string(theme.Warning),
		Muted:     string(theme.Muted),
		Text:      string(theme.Text),
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
