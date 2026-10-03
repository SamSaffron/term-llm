package uv

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRendererWideRowRepaintDisablesAutowrap(t *testing.T) {
	for _, fullscreen := range []bool{false, true} {
		for _, relative := range []bool{false, true} {
			t.Run(fmt.Sprintf("fullscreen=%v/relative=%v", fullscreen, relative), func(t *testing.T) {
				var out bytes.Buffer
				r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
				r.SetFullscreen(fullscreen)
				r.SetRelativeCursor(relative)
				frame := NewScreenBuffer(6, 2)
				for _, step := range []string{"initial", "changed", "resize", "erase"} {
					out.Reset()
					content := "世abcd\nnext"
					if step == "changed" {
						content = "界abcd\nnext"
					}
					if step == "resize" {
						frame.Resize(5, 2)
						content = "世abc\nnext"
					}
					if step == "erase" {
						r.Erase()
						content = "世abc\nnext"
					}
					NewStyledString(content).Draw(frame, frame.Bounds())
					flushTestFrame(t, r, frame.RenderBuffer)
					output := out.String()
					glyph := strings.IndexAny(output, "世界")
					off := strings.Index(output, ansi.ResetModeAutoWrap)
					on := strings.Index(output, ansi.SetModeAutoWrap)
					if glyph < 0 || off < 0 || off > glyph || on < glyph {
						t.Fatalf("%s: wide non-last row was not painted inside an autowrap guard: %q", step, output)
					}
					if strings.Count(output, ansi.ResetModeAutoWrap) != strings.Count(output, ansi.SetModeAutoWrap) {
						t.Fatalf("%s: autowrap was not restored: %q", step, output)
					}
					if !strings.Contains(output[on+len(ansi.SetModeAutoWrap):], "\r") {
						t.Fatalf("%s: guarded row was not reanchored: %q", step, output)
					}
				}
				out.Reset()
				flushTestFrame(t, r, frame.RenderBuffer)
				if out.Len() != 0 {
					t.Fatalf("unchanged wide frame emitted output: %q", out.String())
				}
			})
		}
	}
}

// Disabling wrap around a single-column combining cluster at the corner
// moves its combining mark onto the previous cell on legacy terminals.
func TestRendererCornerCombiningClusterKeepsAutowrap(t *testing.T) {
	for _, row := range []int{0, 1} {
		for _, combining := range []bool{false, true} {
			t.Run(fmt.Sprintf("row=%d/combining=%v", row, combining), func(t *testing.T) {
				var out bytes.Buffer
				r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
				r.SetFullscreen(true)
				frame := NewScreenBuffer(3, 2)
				// The runtime can select grapheme width; the legacy decoder
				// drops standalone combining marks rather than forming this cell.
				frame.Method = ansi.GraphemeWidth
				content := "abé"
				if combining {
					content += "\u0301"
				}
				NewStyledString(content).Draw(frame, Rect(0, row, 3, 1))
				flushTestFrame(t, r, frame.RenderBuffer)
				wantGuard := row == 1 && !combining
				if strings.Contains(out.String(), ansi.ResetModeAutoWrap) != wantGuard {
					t.Fatalf("corner autowrap guard = %q, want guard %v", out.String(), wantGuard)
				}
			})
		}
	}
}

// The ordinary corner-cell guard must not turn wrapping back on midway
// through a guarded row; this matters when the last row contains a wide cell.
func TestRendererWideCornerUsesSingleAutowrapGuard(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	frame := NewScreenBuffer(6, 1)
	NewStyledString("世abcd").Draw(frame, frame.Bounds())
	flushTestFrame(t, r, frame.RenderBuffer)
	if strings.Count(out.String(), ansi.ResetModeAutoWrap) != 1 || strings.Count(out.String(), ansi.SetModeAutoWrap) != 1 {
		t.Fatalf("wide corner nested an autowrap guard: %q", out.String())
	}
}

func TestRendererGuardedWideRowKeepsMarginClusterAutowrap(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	frame := NewScreenBuffer(3, 1)
	frame.Method = ansi.GraphemeWidth
	NewStyledString("世é\u0301").Draw(frame, frame.Bounds())
	flushTestFrame(t, r, frame.RenderBuffer)
	if !strings.Contains(out.String(), ansi.SetModeAutoWrap+"é\u0301"+ansi.ResetModeAutoWrap) {
		t.Fatalf("guarded row kept autowrap off for margin cluster: %q", out.String())
	}
}
