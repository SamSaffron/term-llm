package ui

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/termimage"
)

// RenderInlineImage renders an image for normal inline/scrollback terminal
// output. It returns an empty string on error or when image rendering is disabled.
func RenderInlineImage(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	result, err := termimage.Render(termimage.Request{
		Path:               path,
		Mode:               termimage.ModeScrollback,
		Protocol:           termimage.ProtocolAuto,
		AllowEscapeUploads: true,
	})
	if err != nil || result.Full == "" {
		return ""
	}
	return result.Full
}

// ClearRenderedImages clears the terminal image render cache.
// Call this when starting a new session.
func ClearRenderedImages() {
	termimage.ClearCache()
}
