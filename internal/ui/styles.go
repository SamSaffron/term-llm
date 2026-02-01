package ui

import (
	"os"

	"github.com/charmbracelet/glamour/ansi"
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

	// Message backgrounds
	UserMsgBg lipgloss.Color // background for user messages in chat
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

// GlamourStyle returns a glamour StyleConfig based on the current theme
func GlamourStyle() ansi.StyleConfig {
	return GlamourStyleFromTheme(currentTheme)
}

// GlamourStyleFromTheme creates a glamour StyleConfig from the given theme
func GlamourStyleFromTheme(theme *Theme) ansi.StyleConfig {
	// Convert lipgloss colors to string pointers
	primary := string(theme.Primary)
	secondary := string(theme.Secondary)
	success := string(theme.Success)
	warning := string(theme.Warning)
	muted := string(theme.Muted)
	text := string(theme.Text)

	return ansi.StyleConfig{
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockPrefix: "\n",
				BlockSuffix: "\n",
				Color:       &text,
			},
			Margin: uintPtr(2),
		},
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color:  &warning,
				Italic: boolPtr(true),
			},
			Indent: uintPtr(2),
		},
		List: ansi.StyleList{
			LevelIndent: 2,
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{
					Color: &text,
				},
			},
		},
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockSuffix: "\n",
				Color:       &secondary,
				Bold:        boolPtr(true),
			},
		},
		H1: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "# ",
			},
		},
		H2: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "## ",
			},
		},
		H3: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "### ",
			},
		},
		H4: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "#### ",
			},
		},
		H5: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "##### ",
			},
		},
		H6: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "###### ",
			},
		},
		Strikethrough: ansi.StylePrimitive{
			CrossedOut: boolPtr(true),
		},
		Emph: ansi.StylePrimitive{
			Color:  &warning,
			Italic: boolPtr(true),
		},
		Strong: ansi.StylePrimitive{
			Bold:  boolPtr(true),
			Color: &primary,
		},
		HorizontalRule: ansi.StylePrimitive{
			Color:  &muted,
			Format: "\n--------\n",
		},
		Item: ansi.StylePrimitive{
			BlockPrefix: "• ",
		},
		Enumeration: ansi.StylePrimitive{
			BlockPrefix: ". ",
			Color:       &secondary,
		},
		Task: ansi.StyleTask{
			StylePrimitive: ansi.StylePrimitive{},
			Ticked:         "[✓] ",
			Unticked:       "[ ] ",
		},
		Link: ansi.StylePrimitive{
			Color:     &secondary,
			Underline: boolPtr(true),
		},
		LinkText: ansi.StylePrimitive{
			Color: &primary,
		},
		Image: ansi.StylePrimitive{
			Color:     &secondary,
			Underline: boolPtr(true),
		},
		ImageText: ansi.StylePrimitive{
			Color:  &muted,
			Format: "Image: {{.text}} →",
		},
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color: &primary,
			},
		},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{
					Color: &text,
				},
				Margin: uintPtr(2),
			},
			Chroma: &ansi.Chroma{
				Text: ansi.StylePrimitive{
					Color: &text,
				},
				Comment: ansi.StylePrimitive{
					Color: &muted,
				},
				CommentPreproc: ansi.StylePrimitive{
					Color: &muted,
				},
				Keyword: ansi.StylePrimitive{
					Color: &primary,
				},
				KeywordReserved: ansi.StylePrimitive{
					Color: &primary,
				},
				KeywordNamespace: ansi.StylePrimitive{
					Color: &primary,
				},
				KeywordType: ansi.StylePrimitive{
					Color: &secondary,
				},
				Operator: ansi.StylePrimitive{
					Color: &text,
				},
				Punctuation: ansi.StylePrimitive{
					Color: &text,
				},
				Name: ansi.StylePrimitive{
					Color: &text,
				},
				NameBuiltin: ansi.StylePrimitive{
					Color: &secondary,
				},
				NameTag: ansi.StylePrimitive{
					Color: &primary,
				},
				NameAttribute: ansi.StylePrimitive{
					Color: &success,
				},
				NameClass: ansi.StylePrimitive{
					Color:     &secondary,
					Underline: boolPtr(true),
					Bold:      boolPtr(true),
				},
				NameConstant: ansi.StylePrimitive{
					Color: &secondary,
				},
				NameDecorator: ansi.StylePrimitive{
					Color: &success,
				},
				NameFunction: ansi.StylePrimitive{
					Color: &success,
				},
				LiteralNumber: ansi.StylePrimitive{
					Color: &secondary,
				},
				LiteralString: ansi.StylePrimitive{
					Color: &warning,
				},
				LiteralStringEscape: ansi.StylePrimitive{
					Color: &primary,
				},
				GenericDeleted: ansi.StylePrimitive{
					Color: &muted,
				},
				GenericEmph: ansi.StylePrimitive{
					Italic: boolPtr(true),
				},
				GenericInserted: ansi.StylePrimitive{
					Color: &success,
				},
				GenericStrong: ansi.StylePrimitive{
					Bold: boolPtr(true),
				},
				GenericSubheading: ansi.StylePrimitive{
					Color: &secondary,
				},
				Background: ansi.StylePrimitive{},
			},
		},
		Table: ansi.StyleTable{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{},
			},
			CenterSeparator: stringPtr("┼"),
			ColumnSeparator: stringPtr("│"),
			RowSeparator:    stringPtr("─"),
		},
		DefinitionDescription: ansi.StylePrimitive{
			BlockPrefix: "\n🠶 ",
		},
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func uintPtr(u uint) *uint {
	return &u
}

func stringPtr(s string) *string {
	return &s
}
