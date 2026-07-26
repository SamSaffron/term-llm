package chat

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	renderchat "github.com/samsaffron/term-llm/internal/render/chat"
)

const minStickyPromptCells = 4

type stickyUserPromptHoverAction int

const (
	stickyUserPromptHoverNone stickyUserPromptHoverAction = iota
	stickyUserPromptHoverPrompt
	stickyUserPromptHoverPrevious
	stickyUserPromptHoverForward
)

type userPromptHoverSurface struct {
	anchor      renderchat.UserMessageAnchor
	anchorIndex int
	row         int
	sticky      bool
}

type stickyUserPromptControlLayout struct {
	previousText string
	separator    string
	forwardText  string
	start        int
	previousFrom int
	previousTo   int
	forwardFrom  int
	forwardTo    int
}

func (m *Model) stickyUserPromptAnchorIndex() (renderchat.UserMessageAnchor, int, bool) {
	if m == nil || !m.altScreen || m.viewport.Height() < 2 {
		return renderchat.UserMessageAnchor{}, -1, false
	}
	top := m.viewport.YOffset()
	anchors := m.viewCache.userMessageAnchors
	for i := len(anchors) - 1; i >= 0; i-- {
		if anchors[i].StartLine > top {
			continue
		}
		if anchors[i].EndLine <= top {
			return anchors[i], i, true
		}
		// The nearest prompt has started but has not fully left the viewport.
		// Do not fall back to an older prompt and cover the visible destination.
		return renderchat.UserMessageAnchor{}, -1, false
	}
	return renderchat.UserMessageAnchor{}, -1, false
}

func (m *Model) stickyUserPromptAnchor() (renderchat.UserMessageAnchor, bool) {
	anchor, _, ok := m.stickyUserPromptAnchorIndex()
	return anchor, ok
}

func (m *Model) userPromptSurfaceAt(x, y int) (userPromptHoverSurface, bool) {
	if m == nil || !m.altScreen || m.resizeReflowPending || y < 0 || y >= m.viewport.Height() || x < 0 || x >= m.width || m.dialog.IsOpen() || m.sideQuestion.Visible {
		return userPromptHoverSurface{}, false
	}
	top := m.viewport.YOffset()
	contentLine := top + y
	anchors := m.viewCache.userMessageAnchors
	next := sort.Search(len(anchors), func(i int) bool { return anchors[i].StartLine > contentLine })
	if next > 0 {
		i := next - 1
		anchor := anchors[i]
		if contentLine < anchor.InteractiveEndLine {
			return userPromptHoverSurface{
				anchor:      anchor,
				anchorIndex: i,
				row:         max(0, anchor.StartLine-top),
			}, true
		}
	}
	if y == 0 {
		if anchor, anchorIndex, ok := m.stickyUserPromptAnchorIndex(); ok {
			return userPromptHoverSurface{anchor: anchor, anchorIndex: anchorIndex, row: 0, sticky: true}, true
		}
	}
	return userPromptHoverSurface{}, false
}

func (m *Model) userPromptMaxYOffset() int {
	return max(0, m.viewport.TotalLineCount()-m.viewport.Height())
}

func (m *Model) hasReachableNextUserPrompt(anchorIndex int) bool {
	next := anchorIndex + 1
	return next < len(m.viewCache.userMessageAnchors) && m.viewCache.userMessageAnchors[next].StartLine <= m.userPromptMaxYOffset()
}

func (m *Model) stickyUserPromptControls(anchorIndex int) stickyUserPromptControlLayout {
	width := max(1, m.width)
	forward := "↓ end"
	if m.hasReachableNextUserPrompt(anchorIndex) {
		forward = "↓ next"
	}
	previous := ""
	if anchorIndex > 0 {
		previous = "↑ prev"
	}

	text := forward
	previousOffset := -1
	if previous != "" {
		withPrevious := previous + "   " + forward
		if ansi.StringWidth(withPrevious)+minStickyPromptCells <= width {
			text = withPrevious
			previousOffset = 0
		}
	}
	if ansi.StringWidth(text)+minStickyPromptCells > width {
		text = "↓"
	}

	start := max(0, width-ansi.StringWidth(text))
	separator := ""
	previousText := ""
	forwardText := text
	if previousOffset >= 0 {
		previousText = previous
		separator = "   "
		forwardText = forward
	}
	layout := stickyUserPromptControlLayout{
		previousText: previousText,
		separator:    separator,
		forwardText:  forwardText,
		start:        start,
		previousFrom: -1,
		previousTo:   -1,
		forwardFrom:  start,
		forwardTo:    width,
	}
	if previousOffset >= 0 {
		layout.previousFrom = start
		layout.previousTo = start + ansi.StringWidth(previous)
		layout.forwardFrom = start + ansi.StringWidth(previous+"   ")
	}
	return layout
}

func (m *Model) renderStickyUserPrompt(preview string) string {
	width := max(1, m.width)
	text := "❯ " + strings.Join(strings.Fields(preview), " ")
	theme := m.styles.Theme()
	hovered := m.stickyUserPromptHovered && (!m.userPromptMouseKnown || m.hoveredUserPromptSticky)
	if !hovered {
		text = ansi.Truncate(text, width, "…")
		if pad := width - ansi.StringWidth(text); pad > 0 {
			text += strings.Repeat(" ", pad)
		}
		return lipgloss.NewStyle().Foreground(theme.Muted).Render(text)
	}

	_, anchorIndex, ok := m.stickyUserPromptAnchorIndex()
	if !ok {
		text = ansi.Truncate(text, width, "…")
		if pad := width - ansi.StringWidth(text); pad > 0 {
			text += strings.Repeat(" ", pad)
		}
		return lipgloss.NewStyle().Foreground(theme.Text).Background(theme.UserMsgBg).Render(text)
	}

	controls := m.stickyUserPromptControls(anchorIndex)
	contentWidth := max(0, controls.start-1)
	text = ansi.Truncate(text, contentWidth, "")
	if pad := contentWidth - ansi.StringWidth(text); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	if controls.start > contentWidth {
		text += strings.Repeat(" ", controls.start-contentWidth)
	}
	promptStyle := lipgloss.NewStyle().Foreground(theme.Text)
	controlStyle := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
	separatorStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	if m.stickyUserPromptHoverAction == stickyUserPromptHoverPrompt {
		promptStyle = promptStyle.Background(theme.UserMsgBg)
	}
	previousStyle := controlStyle
	if m.stickyUserPromptHoverAction == stickyUserPromptHoverPrevious {
		previousStyle = previousStyle.Background(theme.UserMsgBg)
	}
	forwardStyle := controlStyle
	if m.stickyUserPromptHoverAction == stickyUserPromptHoverForward {
		forwardStyle = forwardStyle.Background(theme.UserMsgBg)
	}
	return promptStyle.Render(text) +
		previousStyle.Render(controls.previousText) +
		separatorStyle.Render(controls.separator) +
		forwardStyle.Render(controls.forwardText)
}

func (m *Model) renderVisibleUserPromptControls(line string, anchorIndex int) string {
	controls := m.stickyUserPromptControls(anchorIndex)
	theme := m.styles.Theme()
	contentWidth := max(0, controls.start-1)
	left := ansi.Cut(line, 0, contentWidth)
	if pad := contentWidth - ansi.StringWidth(left); pad > 0 {
		left += lipgloss.NewStyle().Background(theme.UserMsgBg).Render(strings.Repeat(" ", pad))
	}
	buttonGap := ""
	if controls.start > contentWidth {
		buttonGap = lipgloss.NewStyle().Background(theme.UserMsgBg).Render(strings.Repeat(" ", controls.start-contentWidth))
	}
	controlStyle := lipgloss.NewStyle().Foreground(theme.Primary).Background(theme.UserMsgBg).Bold(true)
	activeControlStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(theme.Primary).Bold(true)
	previousStyle := controlStyle
	if m.stickyUserPromptHoverAction == stickyUserPromptHoverPrevious {
		previousStyle = activeControlStyle
	}
	forwardStyle := controlStyle
	if m.stickyUserPromptHoverAction == stickyUserPromptHoverForward {
		forwardStyle = activeControlStyle
	}
	separatorStyle := lipgloss.NewStyle().Background(theme.UserMsgBg)
	return left + buttonGap +
		previousStyle.Render(controls.previousText) +
		separatorStyle.Render(controls.separator) +
		forwardStyle.Render(controls.forwardText)
}

func renderedLineBounds(content string, row int) (int, int, bool) {
	if row < 0 || content == "" {
		return 0, 0, false
	}
	start := 0
	for i := 0; i < row; i++ {
		next := strings.IndexByte(content[start:], '\n')
		if next < 0 {
			return 0, 0, false
		}
		start += next + 1
	}
	endOffset := strings.IndexByte(content[start:], '\n')
	if endOffset < 0 {
		return start, len(content), true
	}
	return start, start + endOffset, true
}

func replaceRenderedLine(content string, row int, replacement string) string {
	start, end, ok := renderedLineBounds(content, row)
	if !ok {
		return content
	}
	return content[:start] + replacement + content[end:]
}

func (m *Model) applyStickyUserPrompt(viewOutput string) string {
	anchor, ok := m.stickyUserPromptAnchor()
	if !ok || viewOutput == "" {
		return viewOutput
	}
	return replaceRenderedLine(viewOutput, 0, m.renderStickyUserPrompt(anchor.Preview))
}

func (m *Model) applyUserPromptNavigation(viewOutput string) string {
	m.refreshUserPromptHover()
	viewOutput = m.applyStickyUserPrompt(viewOutput)
	if !m.stickyUserPromptHovered || m.hoveredUserPromptSticky || m.hoveredUserPromptAnchorIndex < 0 || viewOutput == "" {
		return viewOutput
	}
	start, end, ok := renderedLineBounds(viewOutput, m.hoveredUserPromptRow)
	if !ok {
		return viewOutput
	}
	replacement := m.renderVisibleUserPromptControls(viewOutput[start:end], m.hoveredUserPromptAnchorIndex)
	return viewOutput[:start] + replacement + viewOutput[end:]
}

func (m *Model) clearStickyUserPromptHover() {
	m.stickyUserPromptHovered = false
	m.stickyUserPromptHoverAction = stickyUserPromptHoverNone
	m.hoveredUserPromptAnchorIndex = -1
	m.hoveredUserPromptRow = -1
	m.hoveredUserPromptSticky = false
}

func (m *Model) refreshUserPromptHover() {
	m.clearStickyUserPromptHover()
	if !m.userPromptMouseKnown || !m.altScreen || !m.mouseMode || m.autoSendQueue != nil || m.selection.Dragging {
		return
	}
	surface, ok := m.userPromptSurfaceAt(m.userPromptMouseX, m.userPromptMouseY)
	if !ok {
		return
	}
	m.stickyUserPromptHovered = true
	m.hoveredUserPromptAnchorIndex = surface.anchorIndex
	m.hoveredUserPromptRow = surface.row
	m.hoveredUserPromptSticky = surface.sticky
	if !surface.sticky {
		if m.userPromptMouseY != surface.row {
			return
		}
		controls := m.stickyUserPromptControls(surface.anchorIndex)
		switch {
		case controls.previousFrom >= 0 && m.userPromptMouseX >= controls.previousFrom && m.userPromptMouseX < controls.previousTo:
			m.stickyUserPromptHoverAction = stickyUserPromptHoverPrevious
		case m.userPromptMouseX >= controls.forwardFrom && m.userPromptMouseX < controls.forwardTo:
			m.stickyUserPromptHoverAction = stickyUserPromptHoverForward
		}
		return
	}
	if m.userPromptMouseY != surface.row {
		m.stickyUserPromptHoverAction = stickyUserPromptHoverPrompt
		return
	}
	controls := m.stickyUserPromptControls(surface.anchorIndex)
	switch {
	case m.userPromptMouseX < controls.start:
		m.stickyUserPromptHoverAction = stickyUserPromptHoverPrompt
	case controls.previousFrom >= 0 && m.userPromptMouseX >= controls.previousFrom && m.userPromptMouseX < controls.previousTo:
		m.stickyUserPromptHoverAction = stickyUserPromptHoverPrevious
	case m.userPromptMouseX >= controls.forwardFrom && m.userPromptMouseX < controls.forwardTo:
		m.stickyUserPromptHoverAction = stickyUserPromptHoverForward
	}
}

func (m *Model) handleStickyUserPromptMouseHover(msg tea.MouseMsg) {
	motion, ok := msg.(tea.MouseMotionMsg)
	if !ok {
		return
	}
	if motion.Button == tea.MouseNone && m.userPromptMouseKnown && motion.X == m.userPromptMouseX && motion.Y == m.userPromptMouseY && !m.selection.Dragging {
		return
	}
	m.userPromptMouseKnown = true
	m.userPromptMouseX = motion.X
	m.userPromptMouseY = motion.Y
	if motion.Button != tea.MouseNone {
		m.clearStickyUserPromptHover()
		return
	}
	m.refreshUserPromptHover()
}

func (m *Model) handleStickyUserPromptMouseClick(msg tea.MouseMsg) bool {
	click, ok := msg.(tea.MouseClickMsg)
	if !ok || click.Button != tea.MouseLeft || click.X < 0 || click.X >= m.width || click.Y < 0 || click.Y >= m.viewport.Height() {
		return false
	}
	surface, ok := m.userPromptSurfaceAt(click.X, click.Y)
	if !ok {
		return false
	}
	m.userPromptMouseKnown = true
	m.userPromptMouseX = click.X
	m.userPromptMouseY = click.Y
	action := stickyUserPromptHoverPrompt
	controls := m.stickyUserPromptControls(surface.anchorIndex)
	if !surface.sticky {
		if click.Y != surface.row {
			return false
		}
		switch {
		case controls.previousFrom >= 0 && click.X >= controls.previousFrom && click.X < controls.previousTo:
			action = stickyUserPromptHoverPrevious
		case click.X >= controls.forwardFrom && click.X < controls.forwardTo:
			action = stickyUserPromptHoverForward
		default:
			return false
		}
	} else if click.Y == surface.row {
		switch {
		case click.X < controls.start:
			action = stickyUserPromptHoverPrompt
		case controls.previousFrom >= 0 && click.X >= controls.previousFrom && click.X < controls.previousTo:
			action = stickyUserPromptHoverPrevious
		case click.X >= controls.forwardFrom && click.X < controls.forwardTo:
			action = stickyUserPromptHoverForward
		default:
			return true
		}
	}
	m.selection = Selection{}
	switch action {
	case stickyUserPromptHoverPrevious:
		m.viewport.SetYOffset(m.viewCache.userMessageAnchors[surface.anchorIndex-1].StartLine)
	case stickyUserPromptHoverForward:
		if m.hasReachableNextUserPrompt(surface.anchorIndex) {
			m.viewport.SetYOffset(m.viewCache.userMessageAnchors[surface.anchorIndex+1].StartLine)
		} else {
			m.viewport.GotoBottom()
		}
	default:
		m.viewport.SetYOffset(surface.anchor.StartLine)
	}
	m.refreshUserPromptHover()
	return true
}

func (m *Model) jumpUserMessage(direction int) bool {
	if m == nil || !m.altScreen || m.resizeReflowPending || m.dialog.IsOpen() || m.sideQuestion.Visible || direction == 0 {
		return false
	}
	anchors := m.viewCache.userMessageAnchors
	current := m.viewport.YOffset()
	if direction < 0 {
		for i := len(anchors) - 1; i >= 0; i-- {
			if anchors[i].StartLine < current {
				m.selection = Selection{}
				m.viewport.SetYOffset(anchors[i].StartLine)
				m.refreshUserPromptHover()
				return true
			}
		}
		return false
	}
	maxYOffset := m.userPromptMaxYOffset()
	for i := range anchors {
		if anchors[i].StartLine > current && anchors[i].StartLine <= maxYOffset {
			m.selection = Selection{}
			m.viewport.SetYOffset(anchors[i].StartLine)
			m.refreshUserPromptHover()
			return true
		}
	}
	if len(anchors) > 0 && !m.viewport.AtBottom() {
		m.selection = Selection{}
		m.viewport.GotoBottom()
		m.refreshUserPromptHover()
		return true
	}
	return false
}
