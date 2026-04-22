package chat

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/clipboard"
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
	Active   bool       // true once a drag has started
	Anchor   ContentPos // drag start (fixed)
	Cursor   ContentPos // drag end (follows mouse)
	Dragging bool       // true between press and release
}

// Normalized returns the selection range ordered so start <= end.
func (s Selection) Normalized() (start, end ContentPos) {
	if s.Anchor.Line < s.Cursor.Line ||
		(s.Anchor.Line == s.Cursor.Line && s.Anchor.Col <= s.Cursor.Col) {
		return s.Anchor, s.Cursor
	}
	return s.Cursor, s.Anchor
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
		m.copyStatus = "" // Clear stale copy status
		m.selection = Selection{
			Active:   true,
			Anchor:   ContentPos{Line: contentLine, Col: col},
			Cursor:   ContentPos{Line: contentLine, Col: col},
			Dragging: true,
		}
		return true

	case tea.MouseMotionMsg:
		if !m.selection.Dragging {
			return false
		}
		contentLine, col := m.screenToContent(mouse.X, mouse.Y)
		m.selection.Cursor = ContentPos{Line: contentLine, Col: col}
		return true

	case tea.MouseReleaseMsg:
		if !m.selection.Dragging {
			return false
		}
		contentLine, col := m.screenToContent(mouse.X, mouse.Y)
		m.selection.Cursor = ContentPos{Line: contentLine, Col: col}
		m.selection.Dragging = false
		// Keep selection visible; user copies explicitly with ctrl+y
		start, end := m.selection.Normalized()
		if start.Line != end.Line || start.Col != end.Col {
			return true
		}
		// Click with no drag — clear selection
		m.selection = Selection{}
		return true
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

// applySelectionHighlight overlays reverse-video highlighting on the viewport output.
func (m *Model) applySelectionHighlight(viewOutput string) string {
	if !m.selection.Active {
		return viewOutput
	}

	start, end := m.selection.Normalized()
	lines := strings.Split(viewOutput, "\n")
	yOff := m.viewport.YOffset()

	for i, line := range lines {
		contentLine := yOff + i
		if contentLine < start.Line || contentLine > end.Line {
			continue
		}

		if start.Line == end.Line {
			// Single line selection
			lines[i] = ansisafe.ApplyPartialReverseVideo(line, start.Col, end.Col)
		} else if contentLine == start.Line {
			lines[i] = ansisafe.ApplyPartialReverseVideo(line, start.Col, -1)
		} else if contentLine == end.Line {
			lines[i] = ansisafe.ApplyPartialReverseVideo(line, 0, end.Col)
		} else {
			lines[i] = ansisafe.ApplyReverseVideo(line)
		}
	}

	return strings.Join(lines, "\n")
}

// copySelectionToClipboard extracts selected text and copies to clipboard.
func (m *Model) copySelectionToClipboard() tea.Cmd {
	text := m.extractSelectedText()
	if text == "" {
		m.copyStatus = "nothing to copy"
		return nil
	}

	// Try both clipboard paths; succeed if either works
	sysErr := clipboard.CopyText(text)
	oscErr := clipboard.CopyTextOSC52(text)

	n := utf8.RuneCountInString(text)
	switch {
	case sysErr == nil:
		m.copyStatus = fmt.Sprintf("copied %d chars", n)
	case oscErr == nil:
		m.copyStatus = fmt.Sprintf("copied %d chars (osc52)", n)
	default:
		m.copyStatus = fmt.Sprintf("copy failed: sys=%v, osc52=%v", sysErr, oscErr)
	}

	return nil
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
	// Lazily rebuild content lines from stored content string (avoids
	// splitting the full content on every frame — only when selection needs it).
	if m.contentLines == nil && m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}
	if len(m.contentLines) == 0 {
		return ""
	}

	var b strings.Builder
	for line := start.Line; line <= end.Line; line++ {
		if line < 0 || line >= len(m.contentLines) {
			continue
		}
		stripped := strings.TrimRight(ansi.Strip(m.contentLines[line]), " ")
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
