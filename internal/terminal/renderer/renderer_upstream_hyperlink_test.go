package uv

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// Moving between rows resets the pen, which closes any open hyperlink. Every
// row the link covers has to reopen it, or only the first row stays clickable.
//
// Ported from upstream 270558f `fix(renderer): keep wrapped hyperlinks
// clickable on every line`.
func TestRendererHyperlinkReopensOnEachRow(t *testing.T) {
	const w, h = 4, 2
	var buf bytes.Buffer
	r := NewTerminalRenderer(&buf, []string{"TERM=xterm-256color"})
	r.SetColorProfile(colorprofile.TrueColor)

	cellbuf := NewRenderBuffer(w, h)
	link := NewLink("https://example.com")
	for y := range h {
		for x := range w {
			cellbuf.SetCell(x, y, &Cell{Content: "a", Width: 1, Link: link})
		}
	}

	r.Render(cellbuf)
	if err := r.Flush(); err != nil {
		t.Fatalf("failed to flush renderer: %v", err)
	}

	output := buf.String()
	opens := strings.Count(output, ansi.SetHyperlink(link.URL, link.Params))
	if opens != h {
		t.Errorf("expected the link to open once per row (%d), got %d: %q", h, opens, output)
	}
}

// A frame's trailing pen reset closes the open hyperlink on the terminal but
// must also clear the recorded one, otherwise the next frame's identical link
// looks unchanged and is never reopened.
func TestRendererHyperlinkReopensOnNextFrame(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetColorProfile(colorprofile.TrueColor)

	link := NewLink("https://example.com")
	open := ansi.SetHyperlink(link.URL, link.Params)
	frame := NewRenderBuffer(4, 1)
	draw := func(content string) {
		t.Helper()
		out.Reset()
		for x := range 4 {
			frame.SetCell(x, 0, &Cell{Content: content, Width: 1, Link: link})
		}
		flushTestFrame(t, r, frame)
	}

	draw("a")
	if got := strings.Count(out.String(), open); got != 1 {
		t.Fatalf("first frame opened the link %d times, want 1: %q", got, out.String())
	}

	draw("b")
	if !strings.Contains(out.String(), open) {
		t.Fatalf("frame after a pen reset never reopened the link: %q", out.String())
	}
	if got := strings.Count(out.String(), ansi.ResetHyperlink()); got != 1 {
		t.Fatalf("linked frame closed the link %d times, want 1: %q", got, out.String())
	}
}

// The same stale record leaks into a later frame that draws no link at all:
// the terminal holds no open hyperlink, so nothing may be emitted to close one.
func TestRendererHyperlinkClosesOnceAcrossUnlinkedFrame(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetColorProfile(colorprofile.TrueColor)

	link := NewLink("https://example.com")
	open := ansi.SetHyperlink(link.URL, link.Params)
	frame := NewRenderBuffer(4, 1)
	for x := range 4 {
		frame.SetCell(x, 0, &Cell{Content: "a", Width: 1, Link: link})
	}
	flushTestFrame(t, r, frame)

	for x := range 4 {
		frame.SetCell(x, 0, &Cell{Content: "b", Width: 1})
	}
	flushTestFrame(t, r, frame)

	output := out.String()
	if opens, closes := strings.Count(output, open), strings.Count(output, ansi.ResetHyperlink()); opens != closes {
		t.Errorf("hyperlink opens (%d) and closes (%d) do not balance: %q", opens, closes, output)
	}
	if !strings.Contains(output, ansi.ResetHyperlink()) {
		t.Errorf("unlinked frame left the hyperlink open: %q", output)
	}
}
