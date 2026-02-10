package plan

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle ask_user UI if active
	if m.askUserModel != nil {
		cmd := m.askUserModel.UpdateEmbedded(msg)
		if m.askUserModel.IsDone() || m.askUserModel.IsCancelled() {
			// Send result and clean up
			if m.askUserModel.IsCancelled() {
				m.askUserDoneCh <- nil
			} else {
				m.askUserDoneCh <- m.askUserModel.Answers()
			}
			m.askUserModel = nil
			m.askUserDoneCh = nil
			return m, m.spinner.Tick
		}
		if cmd != nil {
			return m, cmd
		}
		return m, nil
	}

	// Handle chat input focus mode
	if m.chatFocused {
		return m.handleChatInput(msg)
	}

	// Handle command mode (for :w, :q, :wq)
	if m.commandMode {
		return m.handleCommandMode(msg)
	}

	// Global keys that work in both modes
	switch msg.String() {
	case "ctrl+c", "ctrl+q":
		// Cancel agent if running
		if m.agentActive && m.streamCancel != nil {
			m.streamCancel()
			m.agentActive = false
			m.agentStreaming = false
			m.setStatus("Agent cancelled")
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit

	case "ctrl+p":
		// Trigger planner agent
		if m.agentActive {
			m.setStatus("Agent already running - press Esc to cancel")
			return m, nil
		}
		return m.triggerPlanner()

	case "ctrl+s":
		// Save document
		return m.saveDocument()

	case "ctrl+a":
		// Toggle activity panel expanded/collapsed
		m.activityExpanded = !m.activityExpanded
		m.recalcEditorHeight()
		return m, nil

	case "ctrl+k":
		// Clear pending prompt queue
		if len(m.pendingPrompts) > 0 {
			m.pendingPrompts = nil
			m.setStatus("Cleared pending queue")
		}
		return m, nil

	case "ctrl+g":
		// Hand off plan to chat agent
		return m.handoff()

	case "ctrl+h":
		// Toggle help overlay
		m.helpVisible = !m.helpVisible
		return m, nil

	case "tab":
		// Toggle focus between editor and chat input
		return m.toggleChatFocus()

	case "esc":
		// Exit command/visual mode or switch to normal mode (vim)
		m.commandMode = false
		m.commandBuffer = ""
		m.visualMode = false
		m.vimMode = true
		m.vimPending = ""
		return m, nil
	}

	// Vim normal mode handling
	if m.vimMode {
		m.noteEditorInput()
		return m.handleVimNormalMode(msg)
	}

	// Insert mode - pass to editor
	m.noteEditorInput()
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	m.syncDocFromEditor()
	return m, cmd
}

// handleChatInput processes keys when chat input is focused.
func (m *Model) handleChatInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		// Submit the instruction
		text := strings.TrimSpace(m.chatInput.Value())
		if text == "" {
			return m, nil
		}

		// Clear the input
		m.chatInput.SetValue("")

		if m.agentActive {
			// Queue the instruction for after current turn
			if len(m.pendingPrompts) >= 10 {
				m.setStatus("Queue full (max 10) - clear with Ctrl+K")
				return m, nil
			}
			m.pendingPrompts = append(m.pendingPrompts, text)
			m.setStatus(fmt.Sprintf("Queued: %s", truncateResult(text, 40)))
		} else {
			// Agent is idle - trigger immediately with this prompt
			return m.triggerPlannerWithPrompt(text)
		}
		return m, nil

	case tea.KeyEsc:
		// Return focus to editor
		return m.toggleChatFocus()

	case tea.KeyTab:
		// Return focus to editor
		return m.toggleChatFocus()

	default:
		// Pass to chat input textarea
		var cmd tea.Cmd
		m.chatInput, cmd = m.chatInput.Update(msg)
		return m, cmd
	}
}

// toggleChatFocus switches focus between editor and chat input.
func (m *Model) toggleChatFocus() (tea.Model, tea.Cmd) {
	m.chatFocused = !m.chatFocused

	if m.chatFocused {
		m.editor.Blur()
		m.chatInput.Focus()
		// Exit vim normal mode so we can type
		m.vimMode = false
	} else {
		m.chatInput.Blur()
		m.editor.Focus()
	}

	return m, nil
}
