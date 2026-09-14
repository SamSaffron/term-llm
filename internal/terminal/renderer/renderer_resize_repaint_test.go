package uv

import (
	"bytes"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFullscreenGrowPaintsNewRows(t *testing.T) {
	var buf bytes.Buffer
	s := NewTerminalRenderer(&buf, []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	})
	s.SetFullscreen(true)
	s.SaveCursor()
	s.Erase()

	first := NewScreenBuffer(10, 2)
	NewStyledString("a\nb").Draw(first, first.Bounds())
	s.Render(first.RenderBuffer)
	if err := s.Flush(); err != nil {
		t.Fatalf("first Flush failed: %v", err)
	}

	buf.Reset()
	second := NewScreenBuffer(10, 5)
	NewStyledString("a\nb\n\n\nZ").Draw(second, second.Bounds())
	s.Render(second.RenderBuffer)
	if err := s.Flush(); err != nil {
		t.Fatalf("second Flush failed: %v", err)
	}

	out := buf.Bytes()
	if !bytes.Contains(out, []byte("Z")) {
		t.Fatalf("second fullscreen frame did not paint the new bottom row: %q", out)
	}
	fullRepaint := []byte(ansi.CursorHomePosition + ansi.EraseEntireScreen)
	if !bytes.Contains(out, fullRepaint) {
		t.Fatalf("fullscreen grow did not fully repaint the screen: %q", out)
	}

	buf.Reset()
	third := NewScreenBuffer(10, 5)
	NewStyledString("a\nb\n\n\nZ").Draw(third, third.Bounds())
	s.Render(third.RenderBuffer)
	if err := s.Flush(); err != nil {
		t.Fatalf("third Flush failed: %v", err)
	}

	if out := buf.Bytes(); bytes.Contains(out, []byte(ansi.EraseEntireScreen)) {
		t.Fatalf("unchanged frame after fullscreen grow repainted again: %q", out)
	}
}

func TestFullscreenWidthShrinkRepaints(t *testing.T) {
	var buf bytes.Buffer
	s := NewTerminalRenderer(&buf, []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	})
	s.SetFullscreen(true)
	s.SaveCursor()
	s.Erase()

	first := NewScreenBuffer(10, 3)
	NewStyledString("abcdefghij\nsecond\nthird").Draw(first, first.Bounds())
	s.Render(first.RenderBuffer)
	if err := s.Flush(); err != nil {
		t.Fatalf("first Flush failed: %v", err)
	}

	buf.Reset()
	second := NewScreenBuffer(6, 3)
	NewStyledString("abcdef\nsecond\nthird").Draw(second, second.Bounds())
	s.Render(second.RenderBuffer)
	if err := s.Flush(); err != nil {
		t.Fatalf("second Flush failed: %v", err)
	}

	fullRepaint := []byte(ansi.CursorHomePosition + ansi.EraseEntireScreen)
	if out := buf.Bytes(); !bytes.Contains(out, fullRepaint) {
		t.Fatalf("fullscreen width shrink did not fully repaint the screen: %q", out)
	}
}

func TestFullscreenShrinkRepaintsStaleRows(t *testing.T) {
	var buf bytes.Buffer
	s := NewTerminalRenderer(&buf, []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	})
	s.SetFullscreen(true)
	s.SaveCursor()
	s.Erase()

	first := NewScreenBuffer(10, 5)
	NewStyledString("a\nb\nc\nd\ne").Draw(first, first.Bounds())
	s.Render(first.RenderBuffer)
	if err := s.Flush(); err != nil {
		t.Fatalf("first Flush failed: %v", err)
	}

	buf.Reset()
	second := NewScreenBuffer(10, 2)
	NewStyledString("a\nb").Draw(second, second.Bounds())
	s.Render(second.RenderBuffer)
	if err := s.Flush(); err != nil {
		t.Fatalf("second Flush failed: %v", err)
	}

	if out := buf.Bytes(); !bytes.Contains(out, []byte("\x1b[2J")) {
		t.Fatalf("fullscreen shrink did not clear stale reflowed rows: %q", out)
	}
}
