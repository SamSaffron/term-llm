package uv

import "github.com/charmbracelet/x/ansi"

// needsRender includes geometry changes even when a fresh frame reports no
// touched cells: its blank rows can still replace content from the old frame.
// Read the touch count after reconciliation, which marks those blank rows.
func (s *TerminalRenderer) needsRender(newbuf *RenderBuffer) bool {
	if s.clear || s.termResized || newbuf.TouchedLines() > 0 {
		return true
	}
	return s.curbuf != nil && s.curbuf.Bounds() != newbuf.Bounds()
}

// reconcileResize discards the retained model when a resize invalidates what it
// believes is on the physical screen, and resizes it otherwise so the diff below
// walks every row of the new screen.
//
// A width change rewraps in emulator-defined ways, and a height change is no
// safer: a grown terminal refills rows from its own scrollback, so a row that
// was blank can return holding content this renderer never painted. Fullscreen
// mode owns every cell it is about to clear, so it repaints completely; inline
// mode shares the screen with preceding output and keeps its partial clear plus
// the post-diff model resize instead.
func (s *TerminalRenderer) reconcileResize(newbuf *RenderBuffer, curWidth, curHeight int) {
	newWidth, newHeight := newbuf.Width(), newbuf.Height()
	// A fresh differently-sized frame may leave old rows blank without ever
	// touching them. Recheck every row in either mode before diffing.
	for y := range newHeight {
		newbuf.TouchLine(0, y, newWidth)
	}
	if !s.flags.Contains(tFullscreen) {
		return
	}
	if newWidth != curWidth || newHeight != curHeight {
		s.clear = true
	}
	s.curbuf.Resize(newWidth, newHeight)
}

// Resize updates tab stops and latches actual terminal size changes. Frame
// dimensions cannot identify duplicate reports: an inline or fullscreen frame
// may deliberately be smaller than the terminal. Keep relative cursor anchoring
// unchanged; the owned caller re-establishes its position on resize.
func (s *TerminalRenderer) Resize(width, height int) {
	if s.tabs != nil {
		s.tabs.Resize(width)
	}
	if width <= 0 || height <= 0 {
		return
	}
	if s.termW > 0 && (width != s.termW || height != s.termH) {
		s.termResized = true
	}
	s.termW, s.termH = width, height
}

// consumeTerminalResize selects the repaint boundary without changing the
// owned fullscreen-only reconciliation or inline post-diff model resize.
func (s *TerminalRenderer) consumeTerminalResize() bool {
	if !s.termResized {
		return false
	}
	s.termResized = false
	if s.flags.Contains(tFullscreen) {
		s.clear = true
		return false
	}
	return true
}

func (s *TerminalRenderer) clearAbandonedRows(newbuf *RenderBuffer, oldHeight int) {
	if s.clear || s.flags.Contains(tFullscreen) || s.cur.X == -1 || s.cur.Y == -1 {
		return
	}
	if oldHeight > newbuf.Height() {
		// The first abandoned row is outside the new frame, not its last row.
		// An empty frame starts at its own row zero, never above prior output.
		s.clearBelow(newbuf, nil, newbuf.Height())
	}
}

// repaintInline restores only the frame's rows after the terminal resized under
// it. Clearing each row makes the retained model truthful before the normal
// diff, including blank rows. Wide cells still use the shared autowrap guard.
func (s *TerminalRenderer) repaintInline(newbuf *RenderBuffer) {
	if newbuf.Bounds().Empty() {
		return
	}
	for y := range newbuf.Height() {
		s.move(newbuf, 0, y)
		s.updatePen(nil)
		s.buf.WriteString(ansi.EraseLineRight)
		s.curbuf.ClearArea(Rect(0, y, s.curbuf.Width(), 1))
		s.transformLine(newbuf, y)
	}
}
