package uv

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// 8b38db3: the unchanged last row must survive the partial inline erase.
func TestRendererInlineShrinkKeepsLastRow(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetRelativeCursor(true)
	r.Resize(6, 10)
	frame := NewRenderBuffer(6, 8)
	frame.SetCell(0, 1, &Cell{Content: "a", Width: 1})
	flushTestFrame(t, r, frame)
	out.Reset()
	frame.Resize(6, 2)
	flushTestFrame(t, r, frame)
	if !strings.Contains(out.String(), ansi.EraseScreenBelow) {
		t.Fatalf("missing abandoned-row erase: %q", out.String())
	}
	if got := r.curbuf.CellAt(0, 1); got == nil || got.Content != "a" {
		t.Fatalf("lost surviving row: %+v; output=%q", got, out.String())
	}
}

// 2c536ce: erase only the rows given up, without erasing and repainting a
// surviving row. Exact bytes also pin the inline boundary and final cursor.
func TestRendererInlineShrinkErasesFromFirstAbandonedRow(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetRelativeCursor(true)
	frame := NewRenderBuffer(6, 4)
	frame.SetCell(0, 1, &Cell{Content: "a", Width: 1})
	frame.SetCell(0, 3, &Cell{Content: "z", Width: 1})
	flushTestFrame(t, r, frame)
	out.Reset()
	frame.Resize(6, 2)
	flushTestFrame(t, r, frame)
	want := "\r\x1bM" + ansi.EraseScreenBelow + "\x1bM"
	if got := out.String(); got != want {
		t.Fatalf("shrink output=%q, want erase below surviving rows %q", got, want)
	}
}

// 3c072e9: a simultaneous width change must not suppress tail erasure.
func TestRendererInlineShrinkErasesAcrossWidthChange(t *testing.T) {
	for _, width := range []int{3, 23} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			var out bytes.Buffer
			r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
			r.SetRelativeCursor(true)
			r.Resize(6, 10)
			frame := NewRenderBuffer(6, 4)
			frame.SetCell(0, 2, &Cell{Content: "a", Width: 1})
			flushTestFrame(t, r, frame)
			out.Reset()
			frame.Resize(width, 2)
			r.Resize(width, 10)
			frame.SetCell(0, 0, &Cell{Content: "b", Width: 1})
			flushTestFrame(t, r, frame)
			if !strings.Contains(out.String(), ansi.EraseScreenBelow) {
				t.Fatalf("abandoned rows not erased: %q", out.String())
			}
		})
	}
}

// e90e227 / af6533f: Render cannot see a terminal-size round trip between
// frames. Even untouched cells have to be restored, in both cursor modes.
func TestRendererTerminalResizeRoundTripRepaints(t *testing.T) {
	for _, fullscreen := range []bool{false, true} {
		for _, relative := range []bool{false, true} {
			for _, size := range [][2]int{{8, 4}, {20, 2}, {23, 4}} {
				t.Run(fmt.Sprintf("fullscreen=%v/relative=%v/size=%v", fullscreen, relative, size), func(t *testing.T) {
					var out bytes.Buffer
					r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
					r.SetFullscreen(fullscreen)
					r.SetRelativeCursor(relative)
					r.Resize(20, 4)
					frame := NewRenderBuffer(20, 3)
					// A full row is rewrapped by a real width shrink. The wide
					// row also checks the owned guarded paint path on restoration.
					for x := range frame.Width() {
						frame.SetCell(x, 0, &Cell{Content: "a", Width: 1})
					}
					frame.SetCell(0, 1, &Cell{Content: "世", Width: 2})
					frame.SetCell(0, 2, &Cell{Content: "z", Width: 1})
					flushTestFrame(t, r, frame)
					// Make the frame genuinely untouched, rather than relying on clean but
					// non-nil LineData records (which TouchedLines still counts).
					frame.Touched = nil
					out.Reset()
					r.Resize(size[0], size[1])
					r.Resize(20, 4)
					flushTestFrame(t, r, frame)
					got := out.String()
					if !strings.Contains(got, "a") || !strings.Contains(got, "z") {
						t.Fatalf("terminal resize left unchanged cells unpainted: %q", got)
					}
					if !strings.Contains(got, ansi.ResetModeAutoWrap+"世") ||
						!strings.Contains(got, ansi.SetModeAutoWrap+"\r") {
						t.Fatalf("resize repaint bypassed the wide-row autowrap guard: %q", got)
					}
					if fullscreen != strings.Contains(got, ansi.EraseEntireScreen) {
						t.Fatalf("repaint crossed ownership boundary: %q", got)
					}
					out.Reset()
					flushTestFrame(t, r, frame)
					if out.Len() != 0 {
						t.Fatalf("resize remained latched after render: %q", out.String())
					}
				})
			}
		}
	}
}

// c0d8ed2: the reported terminal can be taller than the application's frame;
// duplicate reports must not repaint it. The round-trip test above is the
// positive control that actual changes still latch.
func TestRendererDuplicateResizeDoesNotRepaint(t *testing.T) {
	for _, fullscreen := range []bool{false, true} {
		t.Run(fmt.Sprint(fullscreen), func(t *testing.T) {
			var out bytes.Buffer
			r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
			r.SetFullscreen(fullscreen)
			r.Resize(20, 8)
			frame := NewRenderBuffer(20, 6)
			frame.SetCell(0, 0, &Cell{Content: "h", Width: 1})
			flushTestFrame(t, r, frame)
			for range 3 {
				out.Reset()
				r.Resize(20, 8)
				flushTestFrame(t, r, frame)
				if out.Len() != 0 {
					t.Fatalf("duplicate size repainted shorter frame: %q", out.String())
				}
			}
		})
	}
}

// 08f27a0: the owned scrolln already touches all moved rows. Assert emitted
// bytes as well as retained cells, including the unchanged application row.
func TestRendererHardwareScrollRepaintsUntouchedRows(t *testing.T) {
	var out bytes.Buffer
	r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	r.SetScrollOptim(true)
	r.Resize(4, 8)
	frame := NewScreenBuffer(4, 8)
	NewStyledString("aa").Draw(frame, Rect(0, 1, 4, 1))
	NewStyledString("bb").Draw(frame, Rect(0, 2, 4, 1))
	flushTestFrame(t, r, frame.RenderBuffer)
	out.Reset()
	NewStyledString("bb").Draw(frame, Rect(0, 0, 4, 1))
	NewStyledString("aa").Draw(frame, Rect(0, 2, 4, 1))
	flushTestFrame(t, r, frame.RenderBuffer)
	got := out.String()
	if !strings.Contains(got, ansi.ScrollUp(2)) || strings.Count(got, "aa") != 2 {
		t.Fatalf("scroll did not restore both aa rows: %q", got)
	}
	for y := range frame.Height() {
		for x := range frame.Width() {
			if !cellEqual(r.curbuf.CellAt(x, y), frame.CellAt(x, y)) {
				t.Fatalf("retained cell (%d,%d) differs after scroll", x, y)
			}
		}
	}
}

// 6d7982b: an exported touch list can be shortened between frames. Exercise
// both a write that repairs it and a short list entering Render directly.
func TestRendererShortTouchListDoesNotPanic(t *testing.T) {
	for _, touch := range []bool{false, true} {
		t.Run(fmt.Sprint(touch), func(t *testing.T) {
			var out bytes.Buffer
			r := NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
			r.SetFullscreen(true)
			r.SetScrollOptim(true)
			frame := NewRenderBuffer(5, 4)
			frame.SetCell(0, 0, &Cell{Content: "a", Width: 1})
			flushTestFrame(t, r, frame)
			frame.SetCell(0, 0, &Cell{Content: "b", Width: 1})
			frame.Touched = frame.Touched[:1]
			if touch {
				frame.Touched = nil
				frame.SetCell(0, 0, &Cell{Content: "c", Width: 1})
				if len(frame.Touched) < frame.Height() {
					t.Fatalf("touch restored only %d entries for %d rows", len(frame.Touched), frame.Height())
				}
			}
			out.Reset()
			flushTestFrame(t, r, frame)
			want := "b"
			if touch {
				want = "c"
			}
			if !strings.Contains(out.String(), want) {
				t.Fatalf("changed cell not painted: %q", out.String())
			}
		})
	}
}
