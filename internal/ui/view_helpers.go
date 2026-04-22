package ui

import tea "charm.land/bubbletea/v2"

// NewAltScreenView returns a Bubble Tea view configured for alt-screen output.
func NewAltScreenView(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// NewAltScreenMouseView returns an alt-screen view with cell-motion mouse mode enabled.
func NewAltScreenMouseView(content string) tea.View {
	v := NewAltScreenView(content)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
