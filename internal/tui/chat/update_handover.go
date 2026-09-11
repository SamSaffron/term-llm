package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) handleHandoverDone(msg handoverDoneMsg) (tea.Model, tea.Cmd) {
	m.streaming = false
	m.phase = "Thinking"
	// Don't cancel the stream when a tool-initiated handover is pending:
	// the engine must stay alive to receive the tool result.
	if m.handoverToolDoneCh == nil {
		m.releaseStreamCancelFunc()
	}
	if msg.err != nil {
		m.cancelHandoverTool()
		if msg.confirmed {
			m.pendingHandover = nil
		}
		if errors.Is(msg.err, context.Canceled) || errors.Is(msg.err, context.DeadlineExceeded) {
			return m.showFooterMuted("Handover cancelled.")
		}
		return m.showFooterError(fmt.Sprintf("Handover failed: %v", msg.err))
	}
	if msg.result == nil {
		m.cancelHandoverTool()
		if msg.confirmed {
			m.pendingHandover = nil
		}
		return m.showFooterError("Handover failed: no result returned.")
	}
	m.recordHandoverUsage(context.Background(), sessionIDOf(m.sess), msg.result.Model, msg.result.Usage)
	if msg.confirmed {
		if m.pendingHandover == nil {
			m.pendingHandover = &handoverDoneMsg{
				agentName:   msg.agentName,
				providerStr: msg.providerStr,
			}
		}
		m.pendingHandover.result = msg.result
		if instructions := strings.TrimSpace(msg.instructions); instructions != "" {
			m.pendingHandover.instructions = instructions
		}
		return m.executeHandover()
	}
	// Show inline handover confirmation UI
	m.pendingHandover = &msg
	m.handoverPreview = newHandoverPreviewModel(
		msg.result.Document, msg.agentName, msg.providerStr,
		m.width, m.styles,
	)
	m.scrollToBottom = true
	return m, m.terminalTitleCmd()
}

func (m *Model) handleHandoverConfirm(msg handoverConfirmMsg) (tea.Model, tea.Cmd) {
	// Don't signal the tool yet — wait until the handover is actually
	// committed (in executeHandover) so the old turn doesn't resume
	// prematurely if a later step fails.
	// Capture instructions before clearing the preview
	instructions := ""
	if m.handoverPreview != nil {
		instructions = m.handoverPreview.Instructions()
	}
	m.handoverPreview = nil
	if m.pendingHandover == nil {
		m.cancelHandoverTool()
		return m, nil
	}
	m.pendingHandover.instructions = strings.TrimSpace(instructions)
	if m.agentResolver != nil {
		targetAgent, err := m.agentResolver(m.pendingHandover.agentName, m.config)
		if err != nil {
			m.cancelHandoverTool()
			m.pendingHandover = nil
			return m.showFooterError(fmt.Sprintf("Handover failed to resolve target agent: %v", err))
		}
		if targetAgent != nil && strings.TrimSpace(targetAgent.HandoverScript) != "" {
			sourceAgent := handoverSourceAgent(m.pendingHandover, m.agentName)
			return m.startHandoverScriptHandover(targetAgent, sourceAgent, targetAgent, m.pendingHandover.providerStr, true, m.pendingHandover.instructions)
		}
	}
	return m.executeHandover()
}

func (m *Model) handleHandoverCancel(msg handoverCancelMsg) (tea.Model, tea.Cmd) {
	toolWasPending := m.handoverToolDoneCh != nil
	m.cancelHandoverTool()
	m.pendingHandover = nil
	m.handoverPreview = nil
	m.invalidateHistoryCache()
	// Resume the engine stream so the tool result is delivered.
	if toolWasPending {
		m.streaming = true
	}
	return m, m.terminalTitleCmd()
}
