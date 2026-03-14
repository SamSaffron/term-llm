package chat

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
)

func (m *Model) openResumeBrowser() (tea.Model, tea.Cmd) {
	browser := sessionsui.New(m.store, m.width, m.height, m.styles)
	browser.SetEmbedded(true)
	if m.sess != nil {
		browser.SetPreferredSessionID(m.sess.ID)
	}
	if updated, _ := browser.Update(sessionsui.RefreshMsg{}); updated != nil {
		if embedded, ok := updated.(*sessionsui.Model); ok {
			browser = embedded
		}
	}

	m.resumeBrowserMode = true
	m.resumeBrowserModel = browser

	if !m.altScreen {
		return m, tea.EnterAltScreen
	}
	return m, nil
}

func (m *Model) closeResumeBrowser() (tea.Model, tea.Cmd) {
	m.resumeBrowserMode = false
	m.resumeBrowserModel = nil
	m.textarea.Focus()
	if !m.altScreen {
		return m, tea.ExitAltScreen
	}
	return m, nil
}

func (m *Model) requestResumeSession(sessionID string) (tea.Model, tea.Cmd) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return m, nil
	}
	if m.store != nil {
		_ = m.store.SetCurrent(context.Background(), sessionID)
	}
	m.pendingResumeSessionID = sessionID
	m.quitting = true
	return m, tea.Quit
}
