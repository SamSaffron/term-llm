package ui

import (
	"bytes"

	"github.com/charmbracelet/glamour"
	"github.com/samsaffron/term-llm/internal/ui/streaming"
)

// TextSegmentRenderer wraps the streaming markdown renderer for use with
// text segments. It buffers rendered output so View() can read it.
type TextSegmentRenderer struct {
	sr     *streaming.StreamRenderer
	output *bytes.Buffer
	width  int

	// flushedRenderedPos tracks how much of the rendered output has been
	// flushed to scrollback, allowing RenderedFrom to return only unflushed content.
	flushedRenderedPos int
}

// NewTextSegmentRenderer creates a new TextSegmentRenderer with the given width.
// Uses flowing mode (no cursor control) since Bubble Tea owns the terminal.
func NewTextSegmentRenderer(width int) (*TextSegmentRenderer, error) {
	var output bytes.Buffer

	// Use the same style configuration as RenderMarkdown for consistency
	style := GlamourStyle()
	margin := uint(0)
	style.Document.Margin = &margin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = &margin

	// Create streaming renderer with glamour options
	sr, err := streaming.NewRenderer(
		&output,
		glamour.WithStyles(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil, err
	}

	return &TextSegmentRenderer{
		sr:     sr,
		output: &output,
		width:  width,
	}, nil
}

// Write writes text to the streaming renderer.
// Complete markdown blocks are rendered immediately.
func (r *TextSegmentRenderer) Write(text string) error {
	_, err := r.sr.Write([]byte(text))
	return err
}

// Rendered returns the currently rendered output.
// This is the content to display in View().
func (r *TextSegmentRenderer) Rendered() string {
	return r.output.String()
}

// RenderedAll returns the full rendered output including already-flushed content.
func (r *TextSegmentRenderer) RenderedAll() string {
	return r.output.String()
}

// RenderedUnflushed returns only the portion of rendered output that hasn't
// been flushed to scrollback yet. Use this in View() to avoid duplicating
// content that was already printed via FlushStreamingText.
func (r *TextSegmentRenderer) RenderedUnflushed() string {
	output := r.output.String()
	if r.flushedRenderedPos >= len(output) {
		return ""
	}
	return output[r.flushedRenderedPos:]
}

// MarkFlushed marks the current rendered output length as flushed.
// Call this after successfully flushing content to scrollback.
func (r *TextSegmentRenderer) MarkFlushed() {
	r.flushedRenderedPos = r.output.Len()
}

// FlushedRenderedPos returns the current flushed position in the rendered output.
func (r *TextSegmentRenderer) FlushedRenderedPos() int {
	return r.flushedRenderedPos
}

// CommittedMarkdownLen returns the number of raw markdown bytes that have been
// committed as complete blocks by the streaming renderer.
func (r *TextSegmentRenderer) CommittedMarkdownLen() int {
	if r.sr == nil {
		return 0
	}
	return r.sr.CommittedMarkdownLen()
}

// Flush renders any remaining incomplete blocks.
// Call this when the segment is complete.
func (r *TextSegmentRenderer) Flush() error {
	return r.sr.Flush()
}

// Close flushes and cleans up the renderer.
func (r *TextSegmentRenderer) Close() error {
	return r.sr.Close()
}

// Resize handles terminal resize by re-rendering with new width.
// Note: The caller should handle any necessary screen clearing.
func (r *TextSegmentRenderer) Resize(newWidth int) error {
	if newWidth <= 0 || newWidth == r.width {
		return nil
	}

	// Clear current output since we're re-rendering
	r.output.Reset()
	r.flushedRenderedPos = 0 // Reset flush position since buffer is cleared
	r.width = newWidth

	return r.sr.Resize(newWidth)
}

// Width returns the current terminal width.
func (r *TextSegmentRenderer) Width() int {
	return r.width
}
