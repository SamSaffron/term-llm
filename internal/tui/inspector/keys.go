package inspector

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines keybindings for the inspector
type KeyMap struct {
	Quit          key.Binding
	ScrollUp      key.Binding
	ScrollDown    key.Binding
	PageUp        key.Binding
	PageDown      key.Binding
	HalfPageUp    key.Binding
	HalfPageDown  key.Binding
	GoToTop       key.Binding
	GoToBottom    key.Binding
	ExpandVisible key.Binding
}

// DefaultKeyMap returns the default keybindings for the inspector
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit: key.NewBinding(
			key.WithKeys("q", "esc"),
			key.WithHelp("q/esc", "close"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("k", "up"),
			key.WithHelp("k/up", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("j", "down"),
			key.WithHelp("j/down", "scroll down"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdn", "page down"),
		),
		HalfPageUp: key.NewBinding(
			key.WithKeys("ctrl+u", "u"),
			key.WithHelp("ctrl+u/u", "half page up"),
		),
		HalfPageDown: key.NewBinding(
			key.WithKeys("ctrl+d", "d"),
			key.WithHelp("ctrl+d/d", "half page down"),
		),
		GoToTop: key.NewBinding(
			key.WithKeys("g"),
			key.WithHelp("g", "go to top"),
		),
		GoToBottom: key.NewBinding(
			key.WithKeys("G"),
			key.WithHelp("G", "go to bottom"),
		),
		ExpandVisible: key.NewBinding(
			key.WithKeys("e"),
			key.WithHelp("e", "toggle expand"),
		),
	}
}

// ShortHelp returns keybindings for the short help view
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Quit, k.ScrollUp, k.ScrollDown, k.GoToTop, k.GoToBottom}
}

// FullHelp returns keybindings for the full help view
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.ScrollUp, k.ScrollDown, k.PageUp, k.PageDown},
		{k.HalfPageUp, k.HalfPageDown, k.GoToTop, k.GoToBottom},
		{k.ExpandVisible, k.Quit},
	}
}
