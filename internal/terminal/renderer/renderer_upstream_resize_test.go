package uv

import (
	"bytes"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The renderer sees the original geometry again, so the buffer must report
// cells discarded and recreated between frames, even when nobody draws them.
func TestRendererResizeBetweenFramesErasesRecreatedCells(t *testing.T) {
	for _, fullscreen := range []bool{false, true} {
		for _, dimension := range []string{"height", "width"} {
			name := dimension + "/inline"
			if fullscreen {
				name = dimension + "/fullscreen"
			}
			t.Run(name, func(t *testing.T) {
				var out bytes.Buffer
				r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
				r.SetFullscreen(fullscreen)
				r.SetRelativeCursor(true)
				r.Resize(5, 5)
				frame := NewRenderBuffer(5, 5)
				x, y := 0, 3
				if dimension == "width" {
					x, y = 3, 0
				}
				frame.SetCell(x, y, &Cell{Content: "X", Width: 1})
				frame.SetCell(0, 4, &Cell{Content: "Z", Width: 1})
				flushTestFrame(t, r, frame)
				out.Reset()

				if dimension == "height" {
					frame.Resize(5, 2)
				} else {
					frame.Resize(2, 5)
				}
				frame.Resize(5, 5)
				frame.SetCell(0, 4, &Cell{Content: "Z", Width: 1})
				flushTestFrame(t, r, frame)

				output := out.String()
				// The first frame ends after Z at (1,4). The height case
				// erases X at (0,3); the width case overwrites (1..3,0).
				// Neither may touch the unchanged Z on row four.
				want := "\r\x1bM" + ansi.EraseLineRight
				if dimension == "width" {
					want = "\x1b[4A   "
				}
				if output != want {
					t.Fatalf("recreated-cell erase output = %q, want %q", output, want)
				}
				if got := r.curbuf.CellAt(x, y); !cellEqual(got, &EmptyCell) {
					t.Fatalf("retained cell (%d,%d) = %+v, want blank", x, y, got)
				}
				out.Reset()
				flushTestFrame(t, r, frame)
				if out.Len() != 0 {
					t.Fatalf("unchanged frame emitted another repaint: %q", out.String())
				}
			})
		}
	}
}

func TestRendererNewGeometryRechecksUntouchedBlankRows(t *testing.T) {
	for _, draw := range []bool{false, true} {
		name := "blank frame"
		if draw {
			name = "partially touched frame"
		}
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
			r.SetRelativeCursor(true)
			first := NewRenderBuffer(5, 3)
			first.SetCell(0, 0, &Cell{Content: "X", Width: 1})
			flushTestFrame(t, r, first)
			out.Reset()

			second := NewRenderBuffer(10, 6)
			if draw {
				second.SetCell(0, 1, &Cell{Content: "Y", Width: 1})
			}
			flushTestFrame(t, r, second)
			want := "\r" + ansi.EraseScreenBelow + "\n\n\n\n\n"
			if draw {
				want = "\r\n\n\x1b[J\x1b[2A\x1b[K\nY\x1b[K\r\n\x1b[K\n\n\n"
			}
			if got := out.String(); got != want {
				t.Fatalf("geometry erase output = %q, want %q", got, want)
			}
			if got := r.curbuf.CellAt(0, 0); !cellEqual(got, &EmptyCell) {
				t.Fatalf("retained row 0 still contains %+v", got)
			}
		})
	}
}

func flushTestFrame(t *testing.T, r *TerminalRenderer, frame *RenderBuffer) {
	t.Helper()
	r.Render(frame)
	if err := r.Flush(); err != nil {
		t.Fatalf("flush frame: %v", err)
	}
}
