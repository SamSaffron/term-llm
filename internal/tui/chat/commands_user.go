package chat

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/term-llm/term-llm/internal/authoridentity"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
)

// SetUserSelection shares invocation identity across session-model replacements.
func (m *Model) SetUserSelection(user *authoridentity.Selection) { m.user = user }

// SetUser changes the attribution of future turns, never loaded history.
func (m *Model) SetUser(name string) error {
	if m.user == nil {
		m.user = &authoridentity.Selection{}
	}
	return m.user.Set(name)
}

func (m *Model) cmdUser(raw string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	name := strings.TrimSpace(raw)
	if name == "" {
		if current := m.user.Name(); current != "" {
			return m.showFooterMuted("Current user: " + current)
		}
		return m.showFooterMuted("New turns are unattributed. Use /user NAME to set a user.")
	}
	if name == "off" {
		_ = m.SetUser("")
		return m.showFooterMuted("New turns are unattributed.")
	}
	if err := m.SetUser(name); err != nil {
		return m.showFooterError(err.Error())
	}
	return m.showFooterMuted("New turns attributed to " + m.user.Name() + ".")
}

func (m *Model) stampUserTurn(message *core.Message) {
	if err := authoridentity.Stamp(m.rootContext(), m.store, message, m.user.Name()); err != nil {
		m.SetFooterWarning(fmt.Sprintf("Could not record user: %v", err))
	}
}

func (m *Model) newUserTurn(parts []core.Part, visibleText string) *session.Message {
	message := core.Message{Role: core.RoleUser, Parts: parts}
	m.stampUserTurn(&message)
	stored := session.NewMessage(m.sess.ID, message, -1)
	stored.TextContent = visibleText
	return stored
}

// Stamp at admission, not consumption: /user may change while steering waits.
func (m *Model) newSteeringUserTurn(content string, parts []core.Part) core.Message {
	leadingImages := 0
	for leadingImages < len(parts) && parts[leadingImages].Type == core.PartImage {
		leadingImages++
	}
	message := core.Message{Role: core.RoleUser, Parts: make([]core.Part, 0, len(parts)+1)}
	message.Parts = append(message.Parts, parts[:leadingImages]...)
	if content != "" {
		message.Parts = append(message.Parts, core.Part{Type: core.PartText, Text: content})
	}
	message.Parts = append(message.Parts, parts[leadingImages:]...)
	if len(message.Parts) == 0 {
		message = core.UserText(content)
	}
	m.stampUserTurn(&message)
	return message
}
