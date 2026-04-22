package ui

import (
	"image/color"
	"os"

	"charm.land/lipgloss/v2"
)

// Theme defines the color palette for the UI
type Theme struct {
	// Primary colors
	Primary   color.Color // main accent color (commands, highlights)
	Secondary color.Color // secondary accent (headers, borders)

	// Semantic colors
	Success color.Color // success states, enabled
	Error   color.Color // error states, disabled
	Warning color.Color // warnings
	Muted   color.Color // dimmed/secondary text
	Text    color.Color // primary text

	// UI element colors
	Spinner    color.Color // loading spinner
	Border     color.Color // borders and dividers
	Background color.Color // background (if needed)

	// Diff backgrounds
	DiffAddBg     color.Color // background for added lines
	DiffRemoveBg  color.Color // background for removed lines
	DiffContextBg color.Color // background for context lines

	// Message backgrounds
	UserMsgBg color.Color // background for user messages in chat
}

// DefaultTheme returns the default color theme (gruvbox)
func DefaultTheme() *Theme {
	return &Theme{
		Primary:       lipgloss.Color("#b8bb26"), // gruvbox green
		Secondary:     lipgloss.Color("#83a598"), // gruvbox aqua
		Success:       lipgloss.Color("#b8bb26"), // gruvbox green
		Error:         lipgloss.Color("#fb4934"), // gruvbox red
		Warning:       lipgloss.Color("#fabd2f"), // gruvbox yellow
		Muted:         lipgloss.Color("#928374"), // gruvbox gray
		Text:          lipgloss.Color("#ebdbb2"), // gruvbox foreground
		Spinner:       lipgloss.Color("#d3869b"), // gruvbox purple
		Border:        lipgloss.Color("#83a598"), // gruvbox aqua (matches secondary)
		Background:    lipgloss.Color(""),        // default/transparent
		DiffAddBg:     lipgloss.Color("#1d2021"), // gruvbox dark bg with green tint
		DiffRemoveBg:  lipgloss.Color("#1d2021"), // gruvbox dark bg with red tint
		DiffContextBg: lipgloss.Color("#1d2021"), // gruvbox dark bg
		UserMsgBg:     lipgloss.Color("#3c3836"), // gruvbox dark gray (subtle bg)
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
	UserMsgBg string
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
	if cfg.UserMsgBg != "" {
		theme.UserMsgBg = lipgloss.Color(cfg.UserMsgBg)
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

// Styles returns styled text helpers
type Styles struct {
	theme *Theme

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
	_ = output
	return &Styles{
		theme: theme,

		Title: lipgloss.NewStyle().
			Bold(true).
			Foreground(theme.Text),

		Subtitle: lipgloss.NewStyle().
			Foreground(theme.Muted),

		Success: lipgloss.NewStyle().
			Foreground(theme.Success),

		Error: lipgloss.NewStyle().
			Foreground(theme.Error),

		Muted: lipgloss.NewStyle().
			Foreground(theme.Muted),

		Bold: lipgloss.NewStyle().
			Bold(true),

		Highlighted: lipgloss.NewStyle().
			Bold(true).
			Foreground(theme.Primary),

		TableHeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(theme.Text).
			Padding(0, 1),

		TableCell: lipgloss.NewStyle().
			Padding(0, 1),

		TableBorder: lipgloss.NewStyle().
			Foreground(theme.Border),

		Spinner: lipgloss.NewStyle().
			Foreground(theme.Spinner),

		Command: lipgloss.NewStyle().
			Bold(true).
			Foreground(theme.Primary),

		Footer: lipgloss.NewStyle().
			Foreground(theme.Muted),

		DiffAdd: lipgloss.NewStyle().
			Foreground(theme.Success).
			Background(theme.DiffAddBg),

		DiffRemove: lipgloss.NewStyle().
			Foreground(theme.Error).
			Background(theme.DiffRemoveBg),

		DiffContext: lipgloss.NewStyle().
			Foreground(theme.Muted).
			Background(theme.DiffContextBg),

		DiffHeader: lipgloss.NewStyle().
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
