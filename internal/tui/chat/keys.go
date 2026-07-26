package chat

import "charm.land/bubbles/v2/key"

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
	PageUp          key.Binding
	PageDown        key.Binding
	PreviousMessage key.Binding
	NextMessage     key.Binding

	// Shortcuts
	Help        key.Binding
	SwitchModel key.Binding
	CycleEffort key.Binding
	ToggleWeb   key.Binding
	ToggleYolo  key.Binding
	Clear       key.Binding
	NewSession  key.Binding
	MCPPicker   key.Binding
	Inspector   key.Binding
	ExpandTools key.Binding
	Copy        key.Binding
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
		HistoryUp: key.NewBinding(
			key.WithKeys("up"),
			key.WithHelp("up", "history up"),
		),
		HistoryDown: key.NewBinding(
			key.WithKeys("down"),
			key.WithHelp("down", "history down"),
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
		PreviousMessage: key.NewBinding(
			key.WithKeys("ctrl+up"),
			key.WithHelp("ctrl+up", "previous prompt"),
		),
		NextMessage: key.NewBinding(
			key.WithKeys("ctrl+down"),
			key.WithHelp("ctrl+down", "next prompt"),
		),
		Help: key.NewBinding(
			key.WithKeys("ctrl+/", "ctrl+_", "ctrl+h", "ctrl+?", "ctrl+shift+/"),
			key.WithHelp("ctrl+/", "help"),
		),
		SwitchModel: key.NewBinding(
			key.WithKeys("ctrl+l"),
			key.WithHelp("ctrl+l", "model"),
		),
		CycleEffort: key.NewBinding(
			key.WithKeys("ctrl+r"),
			key.WithHelp("ctrl+r", "cycle effort"),
		),
		ToggleWeb: key.NewBinding(
			key.WithKeys("ctrl+s"),
			key.WithHelp("ctrl+s", "web search"),
		),
		ToggleYolo: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("shift+tab", "approval mode"),
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
			key.WithHelp("ctrl+e", "expand details"),
		),
		Copy: key.NewBinding(
			key.WithKeys("ctrl+y"),
			key.WithHelp("ctrl+y", "copy selection"),
		),
	}
}
