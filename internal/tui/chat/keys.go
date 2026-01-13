package chat

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines keybindings for the chat TUI
type KeyMap struct {
	// Global
	Quit     key.Binding
	Help     key.Binding
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
	ScrollUp   key.Binding
	ScrollDown key.Binding
	PageUp     key.Binding
	PageDown   key.Binding
	GoToTop    key.Binding
	GoToBottom key.Binding
	Copy       key.Binding

	// Shortcuts
	SwitchModel key.Binding
	ToggleWeb   key.Binding
	AttachFile  key.Binding
	Clear       key.Binding
	NewSession  key.Binding
	MCPPicker   key.Binding
}

// DefaultKeyMap returns the default keybindings
func DefaultKeyMap() KeyMap {
	return KeyMap{
		// Global
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
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

		// History navigation
		ScrollUp: key.NewBinding(
			key.WithKeys("k"),
			key.WithHelp("k", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("j"),
			key.WithHelp("j", "scroll down"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdown", "page down"),
		),
		GoToTop: key.NewBinding(
			key.WithKeys("g"),
			key.WithHelp("g", "go to top"),
		),
		GoToBottom: key.NewBinding(
			key.WithKeys("G"),
			key.WithHelp("G", "go to bottom"),
		),
		Copy: key.NewBinding(
			key.WithKeys("y"),
			key.WithHelp("y", "copy"),
		),

		// Shortcuts
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
	}
}
