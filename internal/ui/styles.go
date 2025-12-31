package ui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

// Color palette - consistent across all TUI components
var (
	Green = lipgloss.Color("10") // success, enabled
	Red   = lipgloss.Color("9")  // error, disabled
	Grey  = lipgloss.Color("8")  // muted text
	Blue  = lipgloss.Color("4")  // headers, borders
	White = lipgloss.Color("15") // header text
)

// Status indicators
const (
	EnabledIcon  = "●"
	DisabledIcon = "○"
	SuccessIcon  = "✓"
	FailIcon     = "✗"
)

// Styles returns styled text helpers bound to a renderer
type Styles struct {
	renderer *lipgloss.Renderer

	// Text styles
	Title       lipgloss.Style
	Subtitle    lipgloss.Style
	Success     lipgloss.Style
	Error       lipgloss.Style
	Muted       lipgloss.Style
	Bold        lipgloss.Style
	Highlighted lipgloss.Style

	// Table styles
	TableHeader lipgloss.Style
	TableCell   lipgloss.Style
	TableBorder lipgloss.Style
}

// NewStyles creates a new Styles instance for the given output
func NewStyles(output *os.File) *Styles {
	r := lipgloss.NewRenderer(output)

	return &Styles{
		renderer: r,

		Title: r.NewStyle().
			Bold(true).
			Foreground(White),

		Subtitle: r.NewStyle().
			Foreground(Grey),

		Success: r.NewStyle().
			Foreground(Green),

		Error: r.NewStyle().
			Foreground(Red),

		Muted: r.NewStyle().
			Foreground(Grey),

		Bold: r.NewStyle().
			Bold(true),

		Highlighted: r.NewStyle().
			Bold(true).
			Foreground(Green),

		TableHeader: r.NewStyle().
			Bold(true).
			Foreground(White).
			Padding(0, 1),

		TableCell: r.NewStyle().
			Padding(0, 1),

		TableBorder: r.NewStyle().
			Foreground(Blue),
	}
}

// DefaultStyles returns styles for stderr (default TUI output)
func DefaultStyles() *Styles {
	return NewStyles(os.Stderr)
}

// FormatEnabled returns a styled enabled/disabled indicator
func (s *Styles) FormatEnabled(enabled bool) string {
	if enabled {
		return s.Success.Render(EnabledIcon + " enabled")
	}
	return s.Muted.Render(DisabledIcon + " disabled")
}

// FormatResult returns a styled success/fail result
func (s *Styles) FormatResult(success bool, msg string) string {
	if success {
		return s.Success.Render(SuccessIcon+" ") + msg
	}
	return s.Error.Render(FailIcon+" ") + msg
}

// Truncate shortens a string to maxLen with ellipsis
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
