package chat

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func (m *Model) handleCompletionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		// Enter executes immediately with the selected command
		selected := m.completions.Selected()
		if selected != nil {
			// Capture typed input before clearing. Worktree commands clear their own
			// composer only after validation succeeds so failed commands remain editable.
			input := m.textarea.Value()
			m.completions.Hide()
			clearBeforeExecute := selected.Kind != SlashEntrySkill && !strings.HasPrefix(selected.Name, "worktree")
			if clearBeforeExecute {
				m.setTextareaValue("")
			}

			// Multi-word completion items (e.g., "handover @developer",
			// "mcp start server") already contain the selected arg.
			// Preserve any typed suffix beyond what the completion covers.
			if strings.Contains(selected.Name, " ") {
				// Count words in selected name to find where extra args start
				selectedParts := strings.Fields(selected.Name)
				inputParts := strings.Fields(strings.TrimPrefix(input, "/"))
				// If user typed more words than the selection, keep the extra
				if len(inputParts) > len(selectedParts) {
					extra := strings.Join(inputParts[len(selectedParts):], " ")
					updated, cmd := m.ExecuteCommand("/" + selected.Name + " " + extra)
					return updated, cmd, true
				}
				updated, cmd := m.ExecuteCommand("/" + selected.Name)
				return updated, cmd, true
			}
			// Single-word command: extract any args the user typed
			args := ""
			if idx := strings.Index(input, " "); idx != -1 {
				args = strings.TrimSpace(input[idx+1:])
			}
			if args != "" {
				updated, cmd := m.ExecuteCommand("/" + selected.Name + " " + args)
				return updated, cmd, true
			}
			updated, cmd := m.ExecuteCommand("/" + selected.Name)
			return updated, cmd, true
		}
		return m, nil, true
	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		// Tab completes but doesn't execute (for adding args)
		selected := m.completions.Selected()
		if selected != nil {
			m.setTextareaValue("/" + selected.Name + " ")
			// Re-run completions — commands like /handover may show
			// argument completions (e.g., agent names) at this point
			m.updateCompletions()
			if !m.completions.IsVisible() {
				m.completions.Hide()
			}
		}
		return m, nil, true
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		m.completions.Hide()
		return m, nil, true
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
		m.completions.Update(msg)
		return m, nil, true
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
		m.completions.Update(msg)
		return m, nil, true
	case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
		// Update query on backspace
		value := m.textarea.Value()
		if len(value) > 1 {
			m.setTextareaValue(value[:len(value)-1])
			m.updateCompletions()
		} else if len(value) == 1 {
			m.setTextareaValue("")
			m.completions.Hide()
		}
		return m, nil, true
	default:
		// Add character to query
		if len(msg.String()) == 1 {
			m.setTextareaValue(m.textarea.Value() + msg.String())
			m.updateCompletions()
			return m, nil, true
		}
	}
	return m, nil, false
}
