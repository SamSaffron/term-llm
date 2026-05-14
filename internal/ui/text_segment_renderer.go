package ui

import (
	"bytes"

	rendermarkdown "github.com/samsaffron/term-llm/internal/render/markdown"
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
	renderer := rendermarkdown.NewANSI(rendermarkdown.Config{
		Palette: currentMarkdownPalette(),
		Width:   width,
	})

	sr, err := streaming.NewRenderer(&output, renderer)
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
	prevOutput := r.output.Bytes()
	prevFlushedPos := r.flushedRenderedPos
	if prevFlushedPos > len(prevOutput) {
		prevFlushedPos = len(prevOutput)
		r.flushedRenderedPos = prevFlushedPos
	}

	var prevFlushedPrefix []byte
	if prevFlushedPos > 0 {
		prevFlushedPrefix = append([]byte(nil), prevOutput[:prevFlushedPos]...)
	}

	_, err := r.sr.Write([]byte(text))
	if err != nil {
		return err
	}

	currentOutput := r.output.Bytes()
	currentLen := len(currentOutput)
	if r.flushedRenderedPos > currentLen {
		r.flushedRenderedPos = currentLen
	}
	if prevFlushedPos > 0 && len(currentOutput) > 0 {
		commonPrefix := longestCommonPrefixLenBytes(prevFlushedPrefix, currentOutput)
		if r.flushedRenderedPos > commonPrefix {
			r.flushedRenderedPos = commonPrefix
		}
	}

	return nil
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
	return safeANSISlice(output, r.flushedRenderedPos)
}

// PreviewUnflushed returns the unflushed rendered output plus a raw preview of
// the current incomplete block when it is safe to do so. This keeps prose
// streaming smoothly between markdown block boundaries without forcing a full
// text snapshot refresh in the hot View() path.
func (r *TextSegmentRenderer) PreviewUnflushed() string {
	rendered := r.RenderedUnflushed()
	pending := r.PendingMarkdown()
	if pending == "" || r.PendingIsTable() || r.PendingIsList() {
		return rendered
	}
	if rendered == "" {
		return pending
	}
	return rendered + pending
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

// PendingMarkdown returns the markdown that belongs to the current incomplete block.
func (r *TextSegmentRenderer) PendingMarkdown() string {
	if r.sr == nil {
		return ""
	}
	return r.sr.PendingMarkdown()
}

// PendingIsTable reports whether the current incomplete block is a table.
func (r *TextSegmentRenderer) PendingIsTable() bool {
	if r.sr == nil {
		return false
	}
	return r.sr.PendingIsTable()
}

// PendingIsList reports whether the current incomplete block should be treated
// as a list for preview purposes.
func (r *TextSegmentRenderer) PendingIsList() bool {
	if r.sr == nil {
		return false
	}
	return r.sr.PendingIsList()
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

	// Save the already-flushed rendered prefix before clearing the buffer.
	// After re-rendering we remap the flushed boundary via common-prefix
	// logic (same approach as Write) so content already printed to
	// scrollback is not duplicated.
	prevFlushedPos := r.flushedRenderedPos
	prevOutput := r.output.Bytes()
	if prevFlushedPos > len(prevOutput) {
		prevFlushedPos = len(prevOutput)
	}
	var prevFlushedPrefix []byte
	if prevFlushedPos > 0 {
		prevFlushedPrefix = append([]byte(nil), prevOutput[:prevFlushedPos]...)
	}

	r.output.Reset()
	r.width = newWidth

	if err := r.sr.Resize(newWidth); err != nil {
		r.flushedRenderedPos = 0
		return err
	}

	currentOutput := r.output.Bytes()
	if prevFlushedPos > 0 && len(currentOutput) > 0 {
		r.flushedRenderedPos = longestCommonPrefixLenBytes(prevFlushedPrefix, currentOutput)
	} else {
		r.flushedRenderedPos = 0
	}

	return nil
}

// Width returns the current terminal width.
func (r *TextSegmentRenderer) Width() int {
	return r.width
}

func longestCommonPrefixLenBytes(a, b []byte) int {
	maxLen := len(a)
	if len(b) < maxLen {
		maxLen = len(b)
	}
	for index := 0; index < maxLen; index++ {
		if a[index] != b[index] {
			return index
		}
	}
	return maxLen
}
