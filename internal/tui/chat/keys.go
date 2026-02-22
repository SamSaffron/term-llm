package chat

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines keybindings for the chat TUI
type KeyMap struct {
	// Global
	Quit     key.Binding
	Commands key.Binding

	// Editor
	Send        key.Binding
	Newline     key.Binding
	NewlineAlt  key.Binding
	ClearLine   key.Binding
	DeleteWord  key.Binding
	Cancel      key.Binding
	HistoryUp   key.Binding
	HistoryDown key.Binding
	Tab         key.Binding

	// History navigation
	PageUp   key.Binding
	PageDown key.Binding

	// Shortcuts
	SwitchModel key.Binding
	ToggleWeb   key.Binding
	AttachFile  key.Binding
	Clear       key.Binding
	NewSession  key.Binding
	MCPPicker   key.Binding
	Inspector   key.Binding
	ExpandTools key.Binding
}

// DefaultKeyMap returns the default keybindings
func DefaultKeyMap() KeyMap {
	return KeyMap{
		// Global
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		Commands: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("ctrl+p", "commands"),
		),

		// Editor
		Send: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "send"),
		),
		Newline: key.NewBinding(
			key.WithKeys("ctrl+j"),
			key.WithHelp("ctrl+j", "newline"),
		),
		NewlineAlt: key.NewBinding(
			key.WithKeys("alt+enter", "shift+enter"),
			key.WithHelp("alt+enter", "newline"),
		),
		ClearLine: key.NewBinding(
			key.WithKeys("ctrl+u"),
			key.WithHelp("ctrl+u", "clear line"),
		),
		DeleteWord: key.NewBinding(
			key.WithKeys("ctrl+w"),
			key.WithHelp("ctrl+w", "delete word"),
		),
		Cancel: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "cancel"),
		),
		Tab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "complete"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdown", "page down"),
		),
		SwitchModel: key.NewBinding(
			key.WithKeys("ctrl+l"),
			key.WithHelp("ctrl+l", "model"),
		),
		ToggleWeb: key.NewBinding(
			key.WithKeys("ctrl+s"),
			key.WithHelp("ctrl+s", "web search"),
		),
		AttachFile: key.NewBinding(
			key.WithKeys("ctrl+f"),
			key.WithHelp("ctrl+f", "file"),
		),
		Clear: key.NewBinding(
			key.WithKeys("ctrl+k"),
			key.WithHelp("ctrl+k", "clear"),
		),
		NewSession: key.NewBinding(
			key.WithKeys("ctrl+n"),
			key.WithHelp("ctrl+n", "new session"),
		),
		MCPPicker: key.NewBinding(
			key.WithKeys("ctrl+t"),
			key.WithHelp("ctrl+t", "mcp servers"),
		),
		Inspector: key.NewBinding(
			key.WithKeys("ctrl+o"),
			key.WithHelp("ctrl+o", "inspect"),
		),
		ExpandTools: key.NewBinding(
			key.WithKeys("ctrl+e"),
			key.WithHelp("ctrl+e", "expand tools"),
		),
	}
}
