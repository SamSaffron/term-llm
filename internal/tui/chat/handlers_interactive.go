package chat

import (
	"context"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func (m *Model) handleApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	done := m.approvalModel.UpdateEmbedded(msg)
	if done {
		result := m.approvalModel.Result()
		// Add summary to tracker for display
		if m.tracker != nil && !result.Cancelled {
			m.tracker.AddExternalUIResult(m.approvalModel.RenderSummary())
		}
		// Send result and clean up
		m.approvalDoneCh <- result
		m.approvalModel = nil
		m.approvalDoneCh = nil
		m.approvalIsWorkspace = false
		m.pausedForExternalUI = false
		m.bumpContentVersion()
		return m, m.withTerminalTitleCmd(m.spinner.Tick)
	}
	return m, nil
}

func (m *Model) handleAskUserKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cmd := m.askUserModel.UpdateEmbedded(msg)
	if m.askUserModel.IsDone() || m.askUserModel.IsCancelled() {
		// Add summary to tracker for display
		if m.tracker != nil && !m.askUserModel.IsCancelled() {
			m.tracker.AddExternalUIResult(m.askUserModel.RenderPlainSummary())
		}
		// Send result and clean up
		if m.askUserModel.IsCancelled() {
			m.askUserDoneCh <- nil
		} else {
			m.askUserDoneCh <- m.askUserModel.Answers()
		}
		m.askUserModel = nil
		m.askUserDoneCh = nil
		m.pausedForExternalUI = false
		m.bumpContentVersion()
		return m, m.withTerminalTitleCmd(m.spinner.Tick)
	}
	if cmd != nil {
		return m, cmd
	}
	return m, nil
}

func (m *Model) handleHandoverPreviewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	done, handled := m.handoverPreview.UpdateEmbedded(msg)
	if done {
		if m.handoverPreview.confirmed {
			return m.Update(handoverConfirmMsg{})
		}
		return m.Update(handoverCancelMsg{})
	}
	if handled {
		return m, nil
	}
	// Allow viewport scroll keys to pass through; block everything else
	if m.altScreen && (key.Matches(msg, m.keyMap.PageUp) || key.Matches(msg, m.keyMap.PageDown)) {
		var loadCmd tea.Cmd
		if key.Matches(msg, m.keyMap.PageUp) {
			loadCmd = m.loadOlderScrollbackPrefix(context.Background())
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, tea.Batch(loadCmd, cmd)
	}
	return m, nil
}
