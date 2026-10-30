package uv

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A width grow can restore reflowed content from scrollback too. This is the
// frame-geometry path, without any Resize notification to latch.
func TestRendererFullscreenWidthGrowRepaints(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	frame := NewRenderBuffer(6, 3)
	frame.SetCell(0, 0, &Cell{Content: "a", Width: 1})
	flushTestFrame(t, r, frame)
	out.Reset()
	frame.Resize(10, 3)
	flushTestFrame(t, r, frame)
	if got := out.String(); !strings.Contains(got, ansi.EraseEntireScreen) || !strings.Contains(got, "a") {
		t.Fatalf("width grow did not restore existing content: %q", got)
	}
}

// Inline repaint must erase blank rows as well as replay unchanged text, but
// must not erase above or below its frame. The first frame ends at (1,2).
func TestRendererInlineResizeRepaintsOnlyOwnedRows(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetRelativeCursor(true)
	r.Resize(6, 10)
	frame := NewRenderBuffer(6, 3)
	frame.SetCell(0, 0, &Cell{Content: "a", Width: 1})
	frame.SetCell(0, 2, &Cell{Content: "z", Width: 1})
	flushTestFrame(t, r, frame)
	out.Reset()
	r.Resize(3, 10)
	r.Resize(6, 10)
	flushTestFrame(t, r, frame)
	want := "\r\x1b[2A" + ansi.EraseLineRight + "a\r\n" + ansi.EraseLineRight + "\n" + ansi.EraseLineRight + "z"
	if got := out.String(); got != want {
		t.Fatalf("inline repaint output=%q, want %q", got, want)
	}
}
