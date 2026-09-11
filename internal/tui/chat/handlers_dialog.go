package chat

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/mcp"
)

func (m *Model) handleDialogKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.dialog.Type() {
	case DialogContent:
		m.dialog.Update(msg)
		return m, nil
	case DialogBranchContext:
		return m.handleBranchContextDialogKey(msg)
	case DialogBranchTree:
		return m.handleBranchTreeDialogKey(msg)
	case DialogModelPicker:
		return m.handleModelPickerDialogKey(msg)
	case DialogMCPPicker:
		return m.handleMCPPickerDialogKey(msg)
	case DialogWorktreeRecovery:
		return m.handleWorktreeRecoveryDialogKey(msg)
	default:
		return m.handleStandardDialogKey(msg)
	}
}

func (m *Model) handleBranchContextDialogKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.dialog.BranchFocusEditing() {
		switch {
		case key.Matches(msg, m.keyMap.Send):
			focus := strings.TrimSpace(m.dialog.BranchFocus())
			if focus == "" {
				m.dialog.SetBranchFocusError("Describe what the new path should retain.")
				return m, nil
			}
			m.dialog.Close()
			return m.startConversationBranchWithNotes(focus)
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.dialog.CancelBranchFocus()
			return m, nil
		default:
			_, cmd := m.dialog.Update(msg)
			return m, cmd
		}
	}

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
		selected := m.dialog.Selected()
		if selected == nil {
			return m, nil
		}
		if selected.ID == "focused" {
			return m.handleBranchContextSelection(selected.ID)
		}
		m.dialog.Close()
		return m.handleBranchContextSelection(selected.ID)
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))):
		m.pendingBranchPoint = nil
		m.dialog.Close()
		return m, nil
	default:
		_, cmd := m.dialog.Update(msg)
		return m, cmd
	}
}

func (m *Model) handleBranchTreeDialogKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
		selected := m.dialog.Selected()
		if selected != nil {
			m.dialog.Close()
			return m.handleBranchTreeSelection(selected.ID)
		}
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		if m.dialog.Query() != "" {
			m.dialog.SetQuery("")
			return m, nil
		}
		m.dialog.Close()
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		m.dialog.Close()
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
		m.dialog.SetCursor(m.dialog.Cursor() - 1)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
		m.dialog.SetCursor(m.dialog.Cursor() + 1)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgup"))):
		m.dialog.MoveTreeUserTurn(-1)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown"))):
		m.dialog.MoveTreeUserTurn(1)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("right"))):
		m.dialog.SetSelectedTreeTurnExpanded(true)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("left"))):
		m.dialog.SetSelectedTreeTurnExpanded(false)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+t"))):
		m.dialog.ToggleTreeTools()
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
		runes := []rune(m.dialog.Query())
		if len(runes) > 0 {
			m.dialog.SetQuery(string(runes[:len(runes)-1]))
		}
		return m, nil
	default:
		if len([]rune(msg.String())) == 1 {
			m.dialog.SetQuery(m.dialog.Query() + msg.String())
		}
		return m, nil
	}
}

func (m *Model) handleModelPickerDialogKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
		selected := m.dialog.Selected()
		if selected != nil {
			m.dialog.Close()
			return m.switchModel(selected.ID)
		}
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "ctrl+c"))):
		m.dialog.Close()
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
		m.dialog.Update(msg)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
		m.dialog.Update(msg)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
		// Update query on backspace
		query := m.dialog.Query()
		if len(query) > 0 {
			m.dialog.SetQuery(query[:len(query)-1])
		}
		return m, nil
	default:
		// Type to filter
		if len(msg.String()) == 1 {
			m.dialog.SetQuery(m.dialog.Query() + msg.String())
			return m, nil
		}
	}
	return m, nil
}

func (m *Model) handleMCPPickerDialogKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		selected := m.dialog.Selected()
		if selected != nil {
			// Toggle the selected MCP server and keep the runner-facing
			// selection in sync. Chat turns use mcpStr to opt into the
			// attached discovery planner.
			name := selected.ID
			status, _ := m.mcpManager.ServerStatus(name)
			if status == mcp.StatusAuthRequired {
				return m, m.startMCPOAuthCmd(name, false)
			}
			if status == mcp.StatusReady || status == mcp.StatusStarting {
				if err := m.mcpManager.Disable(name); err == nil {
					m.setMCPServerSelected(name, false)
				}
			} else {
				if err := m.mcpManager.Enable(context.Background(), name); err == nil {
					m.setMCPServerSelected(name, true)
				}
			}
			m.refreshMCPPickerIfOpen()
		}
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "ctrl+c"))):
		m.dialog.Close()
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k", "ctrl+p"))):
		m.dialog.Update(msg)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j", "ctrl+n"))):
		m.dialog.Update(msg)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
		// Update query on backspace
		query := m.dialog.Query()
		if len(query) > 0 {
			m.dialog.SetQuery(query[:len(query)-1])
		}
		return m, nil
	default:
		// Type to filter
		if len(msg.String()) == 1 {
			m.dialog.SetQuery(m.dialog.Query() + msg.String())
			return m, nil
		}
	}
	return m, nil
}

func (m *Model) handleWorktreeRecoveryDialogKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		selected := m.dialog.Selected()
		if selected == nil {
			return m.resolveWorktreeRecoveryPrompt(false)
		}
		return m.resolveWorktreeRecoveryPrompt(selected.ID == "yes")
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))):
		return m.resolveWorktreeRecoveryPrompt(false)
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k", "down", "j"))):
		m.dialog.Update(msg)
		return m, nil
	default:
		return m, nil
	}
}

func (m *Model) handleStandardDialogKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
		selected := m.dialog.Selected()
		if selected != nil {
			switch m.dialog.Type() {
			case DialogSessionList:
				m.dialog.Close()
				return m.cmdResume([]string{selected.ID})
			case DialogBranchTree:
				m.dialog.Close()
				return m.handleBranchTreeSelection(selected.ID)
			case DialogBranchContext:
				m.dialog.Close()
				return m.handleBranchContextSelection(selected.ID)
			case DialogShareChoice:
				req := m.pendingShare
				m.pendingShare = nil
				m.dialog.Close()
				if req == nil || selected.ID == "cancel" {
					return m, nil
				}
				if selected.ID == "new" || selected.ID == "create" {
					req.forceNew = true
					return m.startShare(*req, false)
				}
				return m.startShare(*req, true)
			case DialogDirApproval:
				if selected.ID == "__deny__" {
					m.pendingFilePath = ""
					m.dialog.Close()
					return m.showSystemMessage("File access denied.")
				}
				// Approve the directory
				if err := m.approvedDirs.AddDirectory(selected.ID); err != nil {
					m.dialog.Close()
					return m.showSystemMessage("Failed to approve directory: " + err.Error())
				}
				// Now try to attach the file again
				filePath := m.pendingFilePath
				m.pendingFilePath = ""
				m.dialog.Close()
				return m.attachFile(filePath)
			}
		}
		m.dialog.Close()
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))):
		m.pendingFilePath = ""
		m.pendingShare = nil
		if m.dialog.Type() == DialogBranchContext {
			m.pendingBranchPoint = nil
		}
		if m.dialog.Type() == DialogWorktreeRecovery {
			return m.resolveWorktreeRecoveryPrompt(false)
		}
		m.dialog.Close()
		return m, nil
	default:
		m.dialog.Update(msg)
		return m, nil
	}
}
