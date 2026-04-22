package chat

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/ui"
)

const (
	handoverOptionConfirm = iota
	handoverOptionInstructions
	handoverOptionCancel
	handoverOptionCount = 3
)

// handoverPreviewModel renders an inline handover confirmation UI.
type handoverPreviewModel struct {
	document    string
	agentName   string
	providerStr string
	width       int
	cursor      int
	styles      *ui.Styles
	done        bool
	confirmed   bool

	// Instructions input mode
	editing      bool
	instructions string
}

func newHandoverPreviewModel(document, agentName, providerStr string, width int, styles *ui.Styles) *handoverPreviewModel {
	return &handoverPreviewModel{
		document:    document,
		agentName:   agentName,
		providerStr: providerStr,
		width:       width,
		styles:      styles,
	}
}

// Instructions returns any extra instructions the user typed.
func (m *handoverPreviewModel) Instructions() string {
	return strings.TrimSpace(m.instructions)
}

// UpdateEmbedded handles key and paste input. Returns (done, handled).
func (m *handoverPreviewModel) UpdateEmbedded(msg tea.Msg) (done bool, handled bool) {
	// Text editing mode for instructions
	if m.editing {
		switch msg := msg.(type) {
		case tea.KeyPressMsg:
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
				// Submit instructions and confirm handover
				m.done = true
				m.confirmed = true
				return true, true
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
				// Cancel editing, go back to option selection
				m.editing = false
				return false, true
			case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
				if len(m.instructions) > 0 {
					m.instructions = m.instructions[:len(m.instructions)-1]
				}
				return false, true
			default:
				if msg.Text != "" {
					m.instructions += msg.Text
					return false, true
				}
				if msg.Code == tea.KeySpace {
					m.instructions += " "
					return false, true
				}
			}
		case tea.PasteMsg:
			if msg.Content != "" {
				m.instructions += msg.Content
				return false, true
			}
		}
		return false, true
	}

	// Option selection mode
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return false, false
	}

	switch {
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("enter"))):
		if m.cursor == handoverOptionInstructions {
			m.editing = true
			return false, true
		}
		m.done = true
		m.confirmed = m.cursor == handoverOptionConfirm
		return true, true
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("esc", "ctrl+c"))):
		m.done = true
		m.confirmed = false
		return true, true
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("up"))):
		if m.cursor > 0 {
			m.cursor--
		}
		return false, true
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("down"))):
		if m.cursor < handoverOptionCount-1 {
			m.cursor++
		}
		return false, true
	}
	return false, false
}

func (m *handoverPreviewModel) View() string {
	if m.done {
		return ""
	}

	theme := m.styles.Theme()
	innerWidth := m.width - 6
	if innerWidth < 40 {
		innerWidth = 40
	}

	primary := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
	muted := lipgloss.NewStyle().Foreground(theme.Muted)

	var b strings.Builder

	// Header
	target := "@" + m.agentName
	if m.providerStr != "" {
		target += " (" + m.providerStr + ")"
	}
	b.WriteString(primary.Render(fmt.Sprintf("Handover to %s", target)))
	b.WriteString("\n\n")

	// Full document
	for _, line := range strings.Split(m.document, "\n") {
		runes := []rune(line)
		if len(runes) > innerWidth {
			line = string(runes[:innerWidth-3]) + "..."
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Options
	labels := []string{"Confirm handover", "Add instructions", "Keep chatting"}
	for i, label := range labels {
		if i == m.cursor {
			b.WriteString(primary.Render("> " + label))
		} else {
			b.WriteString(muted.Render("  " + label))
		}
		b.WriteString("\n")
	}

	// Instructions input area (shown when editing or when instructions exist)
	if m.editing {
		b.WriteString("\n")
		b.WriteString(muted.Render("Instructions for @" + m.agentName + ":"))
		b.WriteString("\n")
		b.WriteString("> " + m.instructions)
		b.WriteString("\u2588") // block cursor
		b.WriteString("\n")
		b.WriteString(muted.Render("Enter to confirm · Esc to go back"))
	} else if m.instructions != "" {
		b.WriteString("\n")
		b.WriteString(muted.Render("Instructions: "))
		b.WriteString(m.instructions)
		b.WriteString("\n")
	}

	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Border).
		Padding(0, 1).
		Width(m.width - 2)

	return "\n" + borderStyle.Render(b.String())
}
