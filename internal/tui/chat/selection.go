package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/samsaffron/term-llm/internal/ui/ansisafe"
)

// ContentPos is a position within the full rendered content.
type ContentPos struct {
	Line int // 0-based line in full content string
	Col  int // 0-based visual column
}

// Selection tracks mouse-drag selection state in the viewport.
type Selection struct {
	Active       bool       // true once a drag has started
	Anchor       ContentPos // drag start (fixed)
	Cursor       ContentPos // drag end (follows mouse)
	Dragging     bool       // true between press and release
	SideQuestion bool       // positions refer to the visible side-question panel
}

// Normalized returns the selection range ordered so start <= end.
func (s Selection) Normalized() (start, end ContentPos) {
	if s.Anchor.Line < s.Cursor.Line ||
		(s.Anchor.Line == s.Cursor.Line && s.Anchor.Col <= s.Cursor.Col) {
		return s.Anchor, s.Cursor
	}
	return s.Cursor, s.Anchor
}

func (m *Model) beginSelection(pos ContentPos, sideQuestion bool) {
	m.selection = Selection{
		Active:       true,
		Anchor:       pos,
		Cursor:       pos,
		Dragging:     true,
		SideQuestion: sideQuestion,
	}
}

func (m *Model) moveSelection(pos ContentPos) bool {
	if !m.selection.Dragging {
		return false
	}
	m.selection.Cursor = pos
	return true
}

func (m *Model) finishSelection(pos ContentPos) bool {
	if !m.moveSelection(pos) {
		return false
	}
	m.selection.Dragging = false
	start, end := m.selection.Normalized()
	if start == end {
		m.selection = Selection{}
	}
	return true
}

// handleSelectionMouse processes mouse events for text selection in the viewport.
// Returns true if the event was consumed.
func (m *Model) handleSelectionMouse(msg tea.MouseMsg) bool {
	mouse := msg.Mouse()
	switch msg.(type) {
	case tea.MouseClickMsg:
		// Only start selection on left button
		if mouse.Button != tea.MouseLeft {
			return false
		}
		if !m.isInViewportArea(mouse.X, mouse.Y) {
			return false
		}
		contentLine, col := m.screenToContent(mouse.X, mouse.Y)
		m.beginSelection(ContentPos{Line: contentLine, Col: col}, false)
		return true

	case tea.MouseMotionMsg:
		contentLine, col := m.screenToContent(mouse.X, mouse.Y)
		return m.moveSelection(ContentPos{Line: contentLine, Col: col})

	case tea.MouseReleaseMsg:
		contentLine, col := m.screenToContent(mouse.X, mouse.Y)
		return m.finishSelection(ContentPos{Line: contentLine, Col: col})
	}

	return false
}

// isInViewportArea checks if screen coordinates are in the viewport.
func (m *Model) isInViewportArea(x, y int) bool {
	return y >= 0 && y < m.viewport.Height()
}

// screenToContent maps screen coordinates to content line and column.
func (m *Model) screenToContent(x, y int) (line, col int) {
	line = m.viewport.YOffset() + y
	col = x
	return
}

// applySelectionHighlight overlays selection styling on rendered lines. lineOffset
// maps output row zero to the selection's content line; columnOffset/lineWidth
// constrain modal selections to their content area rather than their borders.
func applySelectionHighlight(viewOutput string, selection Selection, lineOffset, columnOffset, lineWidth int) string {
	if !selection.Active {
		return viewOutput
	}
	start, end := selection.Normalized()
	lines := strings.Split(viewOutput, "\n")
	lineEnd := -1
	if lineWidth >= 0 {
		lineEnd = columnOffset + lineWidth
	}
	for i, line := range lines {
		contentLine := lineOffset + i
		if contentLine < start.Line || contentLine > end.Line {
			continue
		}
		switch {
		case start.Line == end.Line:
			lines[i] = ansisafe.ApplyPartialReverseVideo(line, columnOffset+start.Col, columnOffset+end.Col)
		case contentLine == start.Line:
			lines[i] = ansisafe.ApplyPartialReverseVideo(line, columnOffset+start.Col, lineEnd)
		case contentLine == end.Line:
			lines[i] = ansisafe.ApplyPartialReverseVideo(line, columnOffset, columnOffset+end.Col)
		case lineWidth >= 0:
			lines[i] = ansisafe.ApplyPartialReverseVideo(line, columnOffset, lineEnd)
		default:
			lines[i] = ansisafe.ApplyReverseVideo(line)
		}
	}
	return strings.Join(lines, "\n")
}

// applySelectionHighlight overlays reverse-video highlighting on the viewport output.
func (m *Model) applySelectionHighlight(viewOutput string) string {
	if !m.selection.Active || m.selection.SideQuestion {
		return viewOutput
	}
	return applySelectionHighlight(viewOutput, m.selection, m.viewport.YOffset(), 0, -1)
}

// copySelectionToClipboard snapshots selected rendered text and routes it through
// the shared asynchronous clipboard result path.
func (m *Model) copySelectionToClipboard() tea.Cmd {
	text := m.extractSelectedText()
	if text == "" {
		return copyStatusCmd("Nothing to copy yet")
	}
	return copyTextCmd(text, "selection")
}

// extractSelectedText returns the text for the current selection by stripping
// ANSI from the rendered content lines and slicing by visual column.
func (m *Model) extractSelectedText() string {
	if !m.selection.Active {
		return ""
	}
	start, end := m.selection.Normalized()
	if start.Line == end.Line && start.Col == end.Col {
		return ""
	}
	// Lazily rebuild base viewport content only when that is the selection source.
	contentLines := m.sideQuestion.selectionLines
	if !m.selection.SideQuestion {
		if m.contentLines == nil && m.viewCache.lastContentStr != "" {
			m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
		}
		contentLines = m.contentLines
	}
	if len(contentLines) == 0 {
		return ""
	}

	var b strings.Builder
	for line := start.Line; line <= end.Line; line++ {
		if line < 0 || line >= len(contentLines) {
			continue
		}
		stripped := strings.TrimRight(ansi.Strip(contentLines[line]), " ")
		w := ansi.StringWidth(stripped)

		if start.Line == end.Line {
			// Single line: extract visual column range
			s := ui.ClampInt(start.Col, 0, w)
			e := ui.ClampInt(end.Col, s, w)
			b.WriteString(cutVisual(stripped, s, e))
		} else if line == start.Line {
			s := ui.ClampInt(start.Col, 0, w)
			b.WriteString(cutVisual(stripped, s, w))
		} else if line == end.Line {
			e := ui.ClampInt(end.Col, 0, w)
			b.WriteString(cutVisual(stripped, 0, e))
		} else {
			b.WriteString(stripped)
		}
		if line < end.Line {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// cutVisual extracts a visual-column range from a plain-text string.
// Uses ansi.Cut which handles wide characters correctly.
func cutVisual(s string, startCol, endCol int) string {
	if startCol >= endCol {
		return ""
	}
	return ansi.Cut(s, startCol, endCol)
}
