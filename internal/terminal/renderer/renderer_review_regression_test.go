package uv

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRendererCornerClusterLeavesNoPendingWrap(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative=%v", relative), func(t *testing.T) {
			var out bytes.Buffer
			r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
			r.SetFullscreen(true)
			r.SetRelativeCursor(relative)
			frame := NewScreenBuffer(3, 2)
			frame.Method = ansi.GraphemeWidth
			NewStyledString("abé\u0301").Draw(frame, Rect(0, 1, 3, 1))
			flushTestFrame(t, r, frame.RenderBuffer)
			if r.atPhantom || r.cur.X >= frame.Width() {
				t.Fatalf("frame leaves pending wrap: cursor=%+v output=%q", r.cur, out.String())
			}
			if !strings.HasSuffix(out.String(), "é\u0301\r") {
				t.Fatalf("corner cluster must be followed immediately by CR: %q", out.String())
			}
		})
	}
}

func TestRendererEmptyGeometryPreservesInlineBoundary(t *testing.T) {
	for _, fullscreen := range []bool{false, true} {
		for _, relative := range []bool{false, true} {
			for _, size := range [][2]int{{0, 0}, {0, 3}, {5, 0}} {
				t.Run(fmt.Sprintf("fullscreen=%v/relative=%v/size=%v", fullscreen, relative, size), func(t *testing.T) {
					var out bytes.Buffer
					r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
					r.SetFullscreen(fullscreen)
					r.SetRelativeCursor(relative)
					first := NewRenderBuffer(5, 3)
					first.SetCell(0, 0, &Cell{Content: "X", Width: 1})
					flushTestFrame(t, r, first)
					out.Reset()
					flushTestFrame(t, r, NewRenderBuffer(size[0], size[1]))
					if r.cur.Y < 0 {
						t.Fatalf("empty geometry moved above owned frame: cursor=%+v output=%q", r.cur, out.String())
					}
					// The last painted cell was at row zero. An inline empty
					// frame must erase from there, never cursor-up into prior output.
					if !fullscreen && out.String() != "\r"+ansi.EraseScreenBelow {
						t.Fatalf("empty inline frame must clear only its owned rows: %q", out.String())
					}
					out.Reset()
					flushTestFrame(t, r, NewRenderBuffer(5, 3))
					if r.cur.Y < 0 || r.cur.X < 0 || r.atPhantom {
						t.Fatalf("restored geometry has invalid cursor: %+v", r.cur)
					}
				})
			}
		}
	}
}

func TestRendererGuardedMarginClusterReanchorsBeforeNextRow(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	frame := NewScreenBuffer(3, 2)
	frame.Method = ansi.GraphemeWidth
	NewStyledString("世é\u0301\nabc").Draw(frame, frame.Bounds())
	flushTestFrame(t, r, frame.RenderBuffer)
	want := ansi.SetModeAutoWrap + "é\u0301" + ansi.ResetModeAutoWrap + ansi.SetModeAutoWrap + "\r\nab"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("margin cluster did not reanchor before following printable row: %q", out.String())
	}
}
