package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
)

func (m *Model) openResumeBrowser() (tea.Model, tea.Cmd) {
	browser := sessionsui.New(m.store, m.width, m.height, m.styles)
	browser.SetEmbedded(true)
	browser.SetFullTextSearch(true)
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

	return m, nil
}

func (m *Model) closeResumeBrowser() (tea.Model, tea.Cmd) {
	m.resumeBrowserMode = false
	m.resumeBrowserModel = nil
	m.textarea.Focus()
	return m, nil
}

func (m *Model) requestResumeSession(sessionID string) (tea.Model, tea.Cmd) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return m, nil
	}
	m.clearSideQuestionHistory()
	if m.queuedBranchSend != nil && m.branchContextInFlight() {
		m.pendingBranchNavigation = &SessionSwitchRequest{SessionID: sessionID}
		if m.sessionSwitcher != nil {
			_, _ = m.closeResumeBrowser()
		}
		return m.showFooterMuted("Finishing the new thread before switching sessions…")
	}
	if m.sessionSwitcher != nil {
		// The switch replaces this model in place; leave browser mode so the
		// interim frames render the conversation, not a stale list.
		_, _ = m.closeResumeBrowser()
	}
	return m.beginSessionSwitch(SessionSwitchRequest{SessionID: sessionID})
}
