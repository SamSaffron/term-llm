package chat

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type wrappedLineSegment struct {
	start int
	end   int
}

func (m *Model) applyFooterLayout(startY int, layout footerLayout) {
	m.textareaBoundsValid = false

	textareaHeight := layout.textareaHeight
	if textareaHeight <= 0 {
		return
	}

	textareaWidth := m.textarea.Width()
	if textareaWidth <= 0 {
		textareaWidth = m.width
	}
	if textareaWidth <= 0 {
		textareaWidth = 1
	}

	m.textareaTopY = startY + layout.textareaOffsetY
	m.textareaBottomY = m.textareaTopY + textareaHeight - 1
	m.textareaLeftX = 0
	m.textareaPromptWidth = lipgloss.Width(m.textarea.Prompt)
	m.textareaRightX = m.textareaLeftX + m.textareaPromptWidth + textareaWidth - 1

	m.textareaEffectiveWidth = textareaWidth
	if m.textareaEffectiveWidth <= 0 {
		m.textareaEffectiveWidth = 1
	}

	m.textareaBoundsValid = true
}

func (m *Model) handleTextareaMouse(msg tea.MouseMsg) bool {
	if !m.textareaBoundsValid {
		return false
	}
	if msg.Button != tea.MouseButtonLeft {
		return false
	}
	if msg.Action != tea.MouseActionPress && msg.Action != tea.MouseActionMotion {
		return false
	}
	if msg.Shift {
		return false
	}
	if msg.Y < m.textareaTopY || msg.Y > m.textareaBottomY {
		return false
	}
	if msg.X < m.textareaLeftX || msg.X > m.textareaRightX {
		return false
	}

	visualRow := msg.Y - m.textareaTopY
	targetX := msg.X - m.textareaLeftX - m.textareaPromptWidth
	if targetX < 0 {
		targetX = 0
	}

	line, col := m.cursorFromVisualPosition(visualRow, targetX)
	m.textarea.Focus()
	m.moveCursorToLine(line)
	m.textarea.SetCursor(col)

	return true
}

func (m *Model) moveCursorToLine(targetLine int) {
	if targetLine < 0 {
		targetLine = 0
	}

	for current := m.textarea.Line(); current < targetLine; current++ {
		m.textarea.CursorDown()
	}
	for current := m.textarea.Line(); current > targetLine; current-- {
		m.textarea.CursorUp()
	}
}

func (m *Model) cursorFromVisualPosition(visualRow, targetX int) (int, int) {
	lines := strings.Split(m.textarea.Value(), "\n")
	if len(lines) == 0 {
		return 0, 0
	}

	if visualRow < 0 {
		visualRow = 0
	}

	remainingRows := visualRow
	for lineIdx, line := range lines {
		segments := wrapLineSegments(line, m.textareaEffectiveWidth)
		if remainingRows < len(segments) {
			seg := segments[remainingRows]
			return lineIdx, columnForSegmentX([]rune(line), seg.start, seg.end, targetX)
		}
		remainingRows -= len(segments)
	}

	lastLineIdx := len(lines) - 1
	return lastLineIdx, len([]rune(lines[lastLineIdx]))
}

func wrapLineSegments(line string, width int) []wrappedLineSegment {
	if width <= 0 {
		width = 1
	}

	runes := []rune(line)
	if len(runes) == 0 {
		return []wrappedLineSegment{{start: 0, end: 0}}
	}

	segments := make([]wrappedLineSegment, 0, 4)
	segmentStart := 0
	segmentWidth := 0

	for i, r := range runes {
		runeWidth := lipgloss.Width(string(r))
		if runeWidth <= 0 {
			runeWidth = 1
		}

		if segmentWidth > 0 && segmentWidth+runeWidth > width {
			segments = append(segments, wrappedLineSegment{start: segmentStart, end: i})
			segmentStart = i
			segmentWidth = 0
		}

		segmentWidth += runeWidth
	}

	segments = append(segments, wrappedLineSegment{start: segmentStart, end: len(runes)})
	return segments
}

func columnForSegmentX(runes []rune, segmentStart, segmentEnd, targetX int) int {
	if targetX <= 0 {
		return segmentStart
	}
	if segmentEnd <= segmentStart {
		return segmentStart
	}

	x := 0
	for i := segmentStart; i < segmentEnd; i++ {
		runeWidth := lipgloss.Width(string(runes[i]))
		if runeWidth <= 0 {
			runeWidth = 1
		}
		if x+runeWidth > targetX {
			return i
		}
		x += runeWidth
	}
	return segmentEnd
}
