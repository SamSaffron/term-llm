package chat

import (
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m *Model) handleResumeFromExternalUI(msg ResumeFromExternalUIMsg) (tea.Model, tea.Cmd) {
	// Resume from external UI (ask_user or approval)
	m.pausedForExternalUI = false

	// Check if there's an ask_user summary to display
	// Add to tracker so it appears in correct order, then flush immediately
	if summary := tools.GetAndClearAskUserResult(); summary != "" && m.tracker != nil {
		m.tracker.AddExternalUIResult(summary)
		// Flush now to ensure it's printed in correct sequence
		if cmd := m.maybeFlushToScrollback(); cmd != nil {
			cmds := []tea.Cmd{m.spinner.Tick}
			m.appendTerminalTitleCmd(&cmds)
			return m, ui.ComposeFlushFirstCommands([]tea.Cmd{cmd}, cmds)
		}
	}

	return m, m.withTerminalTitleCmd(m.spinner.Tick)
}

func (m *Model) handleApprovalRequest(msg ApprovalRequestMsg) (tea.Model, tea.Cmd) {
	// Yolo may resolve ordinary per-action prompts, but it can never make the
	// direct-human primary workspace authority decision.
	if m.isYoloModeActive() && !msg.IsWorkspace {
		msg.DoneCh <- tools.ApprovalResult{Choice: tools.ApprovalChoiceOnce}
		m.pausedForExternalUI = false
		m.approvalModel = nil
		m.approvalDoneCh = nil
		m.approvalIsWorkspace = false
		return m, nil
	}

	m.closeEmbeddedViewsForInteractivePrompt()
	// Render the approval inside Bubble Tea in both alternate-screen and
	// inline modes. Manager-owned runs cannot safely release a stale program's
	// terminal after their session has moved into the background.
	m.pausedForExternalUI = true
	m.approvalDoneCh = msg.DoneCh
	m.approvalIsWorkspace = msg.IsWorkspace
	switch {
	case msg.IsWorkspace:
		m.approvalModel = tools.NewEmbeddedWorkspaceApprovalModel(msg.Path, m.width)
	case msg.IsShell:
		m.approvalModel = tools.NewEmbeddedShellApprovalModel(msg.Path, msg.WorkDir, m.width)
	default:
		m.approvalModel = tools.NewEmbeddedApprovalModel(msg.Path, msg.IsWrite, m.width)
	}
	if m.tracker != nil {
		m.tracker.MarkCurrentTextComplete(func(text string) string {
			return m.renderMarkdown(text)
		})
	}
	m.scrollToBottom = true
	return m, m.terminalTitleCmd()
}

func (m *Model) handleAskUserRequest(msg AskUserRequestMsg) (tea.Model, tea.Cmd) {
	m.closeEmbeddedViewsForInteractivePrompt()
	// As with approvals, the attached model owns this prompt regardless of
	// screen mode so detached runs never manipulate a stale terminal.
	m.pausedForExternalUI = true
	m.askUserDoneCh = msg.DoneCh
	m.askUserModel = tools.NewEmbeddedAskUserModel(msg.Questions, m.width)
	if m.tracker != nil {
		m.tracker.MarkCurrentTextComplete(func(text string) string {
			return m.renderMarkdown(text)
		})
	}
	m.scrollToBottom = true
	return m, m.terminalTitleCmd()
}

func (m *Model) handleHandoverRequest(msg HandoverRequestMsg) (tea.Model, tea.Cmd) {
	m.closeEmbeddedViewsForInteractivePrompt()
	// Tool-initiated handover: trigger the same flow as /handover @agent.
	// Keep streaming paused until the handover is confirmed, cancelled,
	// or fails — the render path needs !m.streaming to show the preview.
	m.handoverToolDoneCh = msg.DoneCh
	m.streaming = false
	return m.cmdHandover([]string{msg.Agent})
}
