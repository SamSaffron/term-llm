package ui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

// Theme defines the color palette for the UI
type Theme struct {
	// Primary colors
	Primary   lipgloss.Color // main accent color (commands, highlights)
	Secondary lipgloss.Color // secondary accent (headers, borders)

	// Semantic colors
	Success lipgloss.Color // success states, enabled
	Error   lipgloss.Color // error states, disabled
	Warning lipgloss.Color // warnings
	Muted   lipgloss.Color // dimmed/secondary text
	Text    lipgloss.Color // primary text

	// UI element colors
	Spinner    lipgloss.Color // loading spinner
	Border     lipgloss.Color // borders and dividers
	Background lipgloss.Color // background (if needed)

	// Diff backgrounds
	DiffAddBg     lipgloss.Color // background for added lines
	DiffRemoveBg  lipgloss.Color // background for removed lines
	DiffContextBg lipgloss.Color // background for context lines
}

// DefaultTheme returns the default color theme
func DefaultTheme() *Theme {
	return &Theme{
		Primary:       lipgloss.Color("10"),      // bright green
		Secondary:     lipgloss.Color("4"),       // blue
		Success:       lipgloss.Color("10"),      // bright green
		Error:         lipgloss.Color("9"),       // bright red
		Warning:       lipgloss.Color("11"),      // yellow
		Muted:         lipgloss.Color("245"),     // light grey
		Text:          lipgloss.Color("15"),      // white
		Spinner:       lipgloss.Color("205"),     // pink/magenta
		Border:        lipgloss.Color("240"),     // grey border
		Background:    lipgloss.Color(""),        // default/transparent
		DiffAddBg:     lipgloss.Color("#1a2f1a"), // dark green tint
		DiffRemoveBg:  lipgloss.Color("#2f1a1a"), // dark red tint
		DiffContextBg: lipgloss.Color("#1a1a1a"), // dark gray
	}
}

// ThemeConfig mirrors the config.ThemeConfig for applying overrides
type ThemeConfig struct {
	Primary   string
	Secondary string
	Success   string
	Error     string
	Warning   string
	Muted     string
	Text      string
	Spinner   string
}

// ThemeFromConfig creates a theme with config overrides applied
func ThemeFromConfig(cfg ThemeConfig) *Theme {
	theme := DefaultTheme()

	// Apply overrides if specified
	if cfg.Primary != "" {
		theme.Primary = lipgloss.Color(cfg.Primary)
	}
	if cfg.Secondary != "" {
		theme.Secondary = lipgloss.Color(cfg.Secondary)
		theme.Border = lipgloss.Color(cfg.Secondary) // border follows secondary
	}
	if cfg.Success != "" {
		theme.Success = lipgloss.Color(cfg.Success)
	}
	if cfg.Error != "" {
		theme.Error = lipgloss.Color(cfg.Error)
	}
	if cfg.Warning != "" {
		theme.Warning = lipgloss.Color(cfg.Warning)
	}
	if cfg.Muted != "" {
		theme.Muted = lipgloss.Color(cfg.Muted)
	}
	if cfg.Text != "" {
		theme.Text = lipgloss.Color(cfg.Text)
	}
	if cfg.Spinner != "" {
		theme.Spinner = lipgloss.Color(cfg.Spinner)
	}

	return theme
}

// currentTheme is the active theme instance
var currentTheme = DefaultTheme()

// GetTheme returns the current active theme
func GetTheme() *Theme {
	return currentTheme
}

// SetTheme sets the current active theme
func SetTheme(t *Theme) {
	currentTheme = t
}

// InitTheme initializes the theme from config
func InitTheme(cfg ThemeConfig) {
	SetTheme(ThemeFromConfig(cfg))
}

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
	theme    *Theme

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

	// UI element styles
	Spinner lipgloss.Style
	Command lipgloss.Style
	Footer  lipgloss.Style

	// Diff styles
	DiffAdd     lipgloss.Style // Added lines (+)
	DiffRemove  lipgloss.Style // Removed lines (-)
	DiffContext lipgloss.Style // Context lines (unchanged)
	DiffHeader  lipgloss.Style // Diff header (@@ ... @@)
}

// NewStyles creates a new Styles instance for the given output
func NewStyles(output *os.File) *Styles {
	return NewStyledWithTheme(output, currentTheme)
}

// NewStyledWithTheme creates styles with a specific theme
func NewStyledWithTheme(output *os.File, theme *Theme) *Styles {
	r := lipgloss.NewRenderer(output)

	return &Styles{
		renderer: r,
		theme:    theme,

		Title: r.NewStyle().
			Bold(true).
			Foreground(theme.Text),

		Subtitle: r.NewStyle().
			Foreground(theme.Muted),

		Success: r.NewStyle().
			Foreground(theme.Success),

		Error: r.NewStyle().
			Foreground(theme.Error),

		Muted: r.NewStyle().
			Foreground(theme.Muted),

		Bold: r.NewStyle().
			Bold(true),

		Highlighted: r.NewStyle().
			Bold(true).
			Foreground(theme.Primary),

		TableHeader: r.NewStyle().
			Bold(true).
			Foreground(theme.Text).
			Padding(0, 1),

		TableCell: r.NewStyle().
			Padding(0, 1),

		TableBorder: r.NewStyle().
			Foreground(theme.Border),

		Spinner: r.NewStyle().
			Foreground(theme.Spinner),

		Command: r.NewStyle().
			Bold(true).
			Foreground(theme.Primary),

		Footer: r.NewStyle().
			Foreground(theme.Muted),

		DiffAdd: r.NewStyle().
			Foreground(theme.Success).
			Background(theme.DiffAddBg),

		DiffRemove: r.NewStyle().
			Foreground(theme.Error).
			Background(theme.DiffRemoveBg),

		DiffContext: r.NewStyle().
			Foreground(theme.Muted).
			Background(theme.DiffContextBg),

		DiffHeader: r.NewStyle().
			Foreground(theme.Secondary).
			Bold(true),
	}
}

// DefaultStyles returns styles for stderr (default TUI output)
func DefaultStyles() *Styles {
	return NewStyles(os.Stderr)
}

// Theme returns the theme used by these styles
func (s *Styles) Theme() *Theme {
	return s.theme
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
