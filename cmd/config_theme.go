package cmd

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var configThemeCmd = &cobra.Command{
	Use:   "theme",
	Short: "Select a UI color theme",
	Long: `Interactively select from predefined color themes.

Use arrow keys to navigate and see a live preview of each theme.
Press enter to select and save, or esc to cancel.

Available themes: gruvbox (default), dracula, nord, solarized, monokai, classic`,
	RunE: configTheme,
}

func init() {
	configCmd.AddCommand(configThemeCmd)
}

func configTheme(cmd *cobra.Command, args []string) error {
	// Get current theme from config (if any)
	cfg, _ := config.Load()
	currentTheme := ""
	if cfg != nil {
		currentTheme = ui.MatchPresetTheme(ui.ThemeConfig{
			Primary:   cfg.Theme.Primary,
			Secondary: cfg.Theme.Secondary,
			Success:   cfg.Theme.Success,
			Error:     cfg.Theme.Error,
			Warning:   cfg.Theme.Warning,
			Muted:     cfg.Theme.Muted,
			Text:      cfg.Theme.Text,
			Spinner:   cfg.Theme.Spinner,
		})
	}

	// Run interactive selector
	selected, err := runThemeSelector(currentTheme)
	if err != nil {
		return err
	}

	if selected == "" {
		return nil // cancelled
	}

	// Get the selected preset
	preset := ui.GetPresetTheme(selected)
	if preset == nil {
		return fmt.Errorf("unknown theme: %s", selected)
	}

	// Save theme to config
	if err := saveThemeToConfig(preset.Config); err != nil {
		return fmt.Errorf("failed to save theme: %w", err)
	}

	// Apply theme immediately
	ui.InitTheme(preset.Config)

	fmt.Printf("Theme set to: %s\n", selected)
	return nil
}

// saveThemeToConfig saves the theme configuration to config.yaml
func saveThemeToConfig(themeConfig ui.ThemeConfig) error {
	configPath, err := config.GetConfigPath()
	if err != nil {
		return err
	}

	// Read existing file or create empty document
	var root yaml.Node
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create new document with empty mapping
			root = yaml.Node{
				Kind: yaml.DocumentNode,
				Content: []*yaml.Node{{
					Kind: yaml.MappingNode,
				}},
			}
		} else {
			return err
		}
	} else {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return err
		}
	}

	// Set each theme field
	fields := []struct{ key, value string }{
		{"theme.primary", themeConfig.Primary},
		{"theme.secondary", themeConfig.Secondary},
		{"theme.success", themeConfig.Success},
		{"theme.error", themeConfig.Error},
		{"theme.warning", themeConfig.Warning},
		{"theme.muted", themeConfig.Muted},
		{"theme.text", themeConfig.Text},
		{"theme.spinner", themeConfig.Spinner},
	}

	for _, f := range fields {
		if err := setYAMLValue(&root, strings.Split(f.key, "."), f.value); err != nil {
			return err
		}
	}

	// Write back
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return err
	}
	encoder.Close()

	return os.WriteFile(configPath, buf.Bytes(), 0644)
}

// themeSelectorModel is the bubbletea model for theme selection
type themeSelectorModel struct {
	presets      []ui.ThemePreset
	cursor       int
	currentTheme string
	selected     string
	cancelled    bool
	width        int
	height       int
}

func newThemeSelectorModel(currentTheme string) themeSelectorModel {
	var presets []ui.ThemePreset
	for _, name := range ui.PresetThemeNames {
		if preset := ui.GetPresetTheme(name); preset != nil {
			presets = append(presets, *preset)
		}
	}

	// Find cursor position for current theme
	cursor := 0
	for i, p := range presets {
		if p.Name == currentTheme {
			cursor = i
			break
		}
	}

	return themeSelectorModel{
		presets:      presets,
		cursor:       cursor,
		currentTheme: currentTheme,
	}
}

func (m themeSelectorModel) Init() tea.Cmd {
	return nil
}

func (m themeSelectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
			if m.cursor > 0 {
				m.cursor--
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
			if m.cursor < len(m.presets)-1 {
				m.cursor++
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			m.selected = m.presets[m.cursor].Name
			return m, tea.Quit
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q", "ctrl+c"))):
			m.cancelled = true
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}

	return m, nil
}

func (m themeSelectorModel) View() tea.View {
	if len(m.presets) == 0 {
		return tea.NewView("No themes available")
	}

	// Get the hovered theme for preview
	hoveredPreset := m.presets[m.cursor]
	previewTheme := ui.ThemeFromConfig(hoveredPreset.Config)

	// Build list column
	var listBuilder strings.Builder
	listBuilder.WriteString(lipgloss.NewStyle().Bold(true).Render("Select Theme"))
	listBuilder.WriteString("\n\n")

	for i, preset := range m.presets {
		cursor := "  "
		if i == m.cursor {
			cursor = "❯ "
		}

		label := preset.Name
		if preset.Name == m.currentTheme {
			label += " (current)"
		}

		if i == m.cursor {
			style := lipgloss.NewStyle().Bold(true).Foreground(previewTheme.Primary)
			listBuilder.WriteString(style.Render(cursor + label))
		} else {
			listBuilder.WriteString(cursor + label)
		}
		listBuilder.WriteString("\n")
	}

	listBuilder.WriteString("\n")
	listBuilder.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render("↑/↓ navigate · enter select · esc cancel"))

	// Build preview column using preview theme colors
	preview := renderThemePreview(previewTheme, hoveredPreset)

	// Combine columns side by side with spacing
	listCol := lipgloss.NewStyle().Width(30).Render(listBuilder.String())

	return tea.NewView(lipgloss.JoinHorizontal(lipgloss.Top, listCol, "  ", preview))
}

// renderThemePreview renders a preview panel showing the theme colors
func renderThemePreview(theme *ui.Theme, preset ui.ThemePreset) string {
	var b strings.Builder

	// Preview border style
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Border).
		Padding(1, 2)

	// Create styles for each color
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Text)
	primaryStyle := lipgloss.NewStyle().Foreground(theme.Primary)
	secondaryStyle := lipgloss.NewStyle().Foreground(theme.Secondary)
	successStyle := lipgloss.NewStyle().Foreground(theme.Success)
	errorStyle := lipgloss.NewStyle().Foreground(theme.Error)
	warningStyle := lipgloss.NewStyle().Foreground(theme.Warning)
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	spinnerStyle := lipgloss.NewStyle().Foreground(theme.Spinner)

	b.WriteString(titleStyle.Render("Preview: "+preset.Name) + "\n")
	b.WriteString(mutedStyle.Render(preset.Description) + "\n\n")

	b.WriteString(primaryStyle.Render("● Primary: highlighted text") + "\n")
	b.WriteString(secondaryStyle.Render("● Secondary: header text") + "\n")
	b.WriteString(successStyle.Render("✓ Success message") + "\n")
	b.WriteString(errorStyle.Render("✗ Error message") + "\n")
	b.WriteString(warningStyle.Render("⚠ Warning message") + "\n")
	b.WriteString(mutedStyle.Render("○ Muted: explanation") + "\n")
	b.WriteString(spinnerStyle.Render("◐ ") + "Spinner indicator\n")
	b.WriteString("\n")
	b.WriteString(primaryStyle.Bold(true).Render("$ sample command") + "\n")

	return borderStyle.Render(b.String())
}

// runThemeSelector runs the interactive theme selector and returns the selected theme name
func runThemeSelector(currentTheme string) (string, error) {
	// Try to use /dev/tty for proper terminal handling
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		tty = nil
	}

	model := newThemeSelectorModel(currentTheme)

	var opts []tea.ProgramOption
	if tty != nil {
		opts = append(opts, tea.WithInput(tty), tea.WithOutput(tty))
	}

	p := tea.NewProgram(model, opts...)
	finalModel, err := p.Run()
	if err != nil {
		return "", err
	}

	if tty != nil {
		tty.Close()
	}

	m := finalModel.(themeSelectorModel)
	if m.cancelled {
		return "", nil
	}
	return m.selected, nil
}
