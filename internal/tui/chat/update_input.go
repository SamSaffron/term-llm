package chat

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) handleWindowSizeMsg(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	widthChanged := m.width > 0 && m.width != msg.Width
	m.applyWindowSize(msg)

	// Bubble Tea clears the alternate screen before Update receives a width
	// resize. Reflowing every historical message synchronously in the following
	// View leaves that cleared screen visible for the entire reflow. Draw a
	// fitted cached frame first and debounce the expensive rebuild. Height-only
	// changes reuse width-dependent history and render immediately.
	if m.altScreen {
		if widthChanged {
			return m, m.beginAltScreenResizeReflow()
		}
		return m, nil
	}
	if len(m.messages) > 0 {
		history := m.renderHistory()
		return m, tea.Sequence(tea.ClearScreen, tea.Println(history))
	}
	return m, tea.ClearScreen
}

func (m *Model) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Open dialogs are modal: route mouse wheel events to scrollable content
	// dialogs before text selection, textarea clicks, or viewport scrolling.
	if m.dialog.IsOpen() && m.dialog.Type() == DialogContent {
		if _, ok := msg.(tea.MouseWheelMsg); ok {
			m.dialog.Update(msg)
			return m, nil
		}
	}
	if m.sessionTransition != nil {
		if m.handleTextareaMouse(msg) {
			return m, nil
		}
		if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseMiddle {
			text, err := readPrimarySelection()
			if err == nil && text != "" {
				return m.handlePasteMsg(tea.PasteMsg{Content: text})
			}
		}
		return m, nil
	}
	if m.sideQuestion.Visible {
		if m.handleSideQuestionMouseWheel(msg) {
			return m, nil
		}
		if m.altScreen && m.handleSideQuestionSelectionMouse(msg) {
			return m, nil
		}
	}
	m.handleStickyUserPromptMouseHover(msg)
	// The sticky user prompt owns the first viewport row while visible.
	if m.handleStickyUserPromptMouseClick(msg) {
		return m, nil
	}
	// Single-clicking a reasoning header toggles just that block. This runs
	// before drag-selection, and only consumes clicks on recognized headers.
	if m.handleReasoningMouseClick(msg) {
		m.selection = Selection{}
		return m, nil
	}
	// Text selection in alt-screen viewport (before textarea handling)
	if m.altScreen && m.handleSelectionMouse(msg) {
		return m, nil
	}
	if m.handleTextareaMouse(msg) {
		return m, nil
	}
	// Handle middle-click paste: read primary selection and route through
	// the PasteMsg path so collapse logic applies.
	if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseMiddle {
		text, err := readPrimarySelection()
		if err == nil && text != "" {
			return m.handlePasteMsg(tea.PasteMsg{Content: text})
		}
		return m, nil
	}
	// Forward mouse events to viewport in alt-screen mode for scroll wheel support.
	// Do not let horizontal wheel/shift-wheel gestures modify the viewport's
	// hidden x-offset; chat history is always rendered at column zero.
	if m.altScreen {
		var loadCmd tea.Cmd
		if mouse := msg.Mouse(); mouse.Button == tea.MouseWheelUp {
			loadCmd = m.loadOlderScrollbackPrefix(context.Background())
		}
		if isHorizontalViewportScroll(msg) {
			m.resetViewportHorizontalOffset()
			return m, loadCmd
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.resetViewportHorizontalOffset()
		return m, tea.Batch(loadCmd, cmd)
	}
	return m, nil
}
