package tuiutil

import (
	"fmt"
	"image/color"

	"charm.land/lipgloss/v2"
)

// AccentPanelStyle returns the standard interactive panel style used by
// term-llm prompts with a left accent border.
func AccentPanelStyle(accent color.Color) lipgloss.Style {
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderLeft(true).
		BorderForeground(accent).
		PaddingLeft(1).
		PaddingRight(2).
		PaddingTop(1).
		PaddingBottom(1)
}

// CompactAccentPanelStyle returns the compact summary panel style used for
// post-action confirmations.
func CompactAccentPanelStyle(accent color.Color) lipgloss.Style {
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderLeft(true).
		BorderForeground(accent).
		PaddingLeft(1).
		PaddingRight(2)
}

// AccentTitleStyle returns the standard bold accent-colored title style.
func AccentTitleStyle(accent color.Color) lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(accent).
		Bold(true).
		MarginBottom(1)
}

// MutedHelpStyle returns the standard muted help-text style.
func MutedHelpStyle(muted color.Color) lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(muted).
		MarginTop(1)
}

// NumberedOptionPrefix returns the standard numbered option prefix for a list item.
func NumberedOptionPrefix(index int, selected bool) string {
	if selected {
		return fmt.Sprintf("> %d. ", index+1)
	}
	return fmt.Sprintf("  %d. ", index+1)
}

// QuickSelectHelpText returns the shared help text for numbered selection prompts.
func QuickSelectHelpText(optionCount int) string {
	if optionCount < 1 {
		optionCount = 1
	}
	return fmt.Sprintf("↑↓ select  1-%d quick  enter confirm  esc cancel", optionCount)
}
