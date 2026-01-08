package chat

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/ui"
)

// CompletionsModel handles the command completions popup
type CompletionsModel struct {
	items    []Command
	filtered []Command
	cursor   int
	query    string
	visible  bool
	width    int
	height   int
	styles   *ui.Styles
}

// NewCompletionsModel creates a new completions model
func NewCompletionsModel(styles *ui.Styles) *CompletionsModel {
	return &CompletionsModel{
		items:    AllCommands(),
		filtered: AllCommands(),
		styles:   styles,
	}
}

// SetSize updates the dimensions
func (c *CompletionsModel) SetSize(width, height int) {
	c.width = width
	c.height = height
}

// Show displays the completions popup
func (c *CompletionsModel) Show() {
	c.visible = true
	c.query = ""
	c.cursor = 0
	c.filtered = c.items
}

// Hide hides the completions popup
func (c *CompletionsModel) Hide() {
	c.visible = false
	c.query = ""
	c.cursor = 0
}

// IsVisible returns whether the popup is visible
func (c *CompletionsModel) IsVisible() bool {
	return c.visible
}

// SetQuery updates the filter query
func (c *CompletionsModel) SetQuery(query string) {
	c.query = query
	c.filtered = FilterCommands(query)
	if c.cursor >= len(c.filtered) {
		c.cursor = max(0, len(c.filtered)-1)
	}
}

// Selected returns the currently selected command
func (c *CompletionsModel) Selected() *Command {
	if len(c.filtered) == 0 {
		return nil
	}
	return &c.filtered[c.cursor]
}

// Update handles messages
func (c *CompletionsModel) Update(msg tea.Msg) (*CompletionsModel, tea.Cmd) {
	if !c.visible {
		return c, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
			if c.cursor > 0 {
				c.cursor--
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
			if c.cursor < len(c.filtered)-1 {
				c.cursor++
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			c.Hide()
		}
	}

	return c, nil
}

// View renders the completions popup
func (c *CompletionsModel) View() string {
	if !c.visible || len(c.filtered) == 0 {
		return ""
	}

	theme := c.styles.Theme()
	maxItems := 10
	items := c.filtered
	if len(items) > maxItems {
		items = items[:maxItems]
	}

	// Find longest command name for alignment
	maxNameLen := 0
	for _, item := range items {
		nameLen := len(item.Name) + 1 // +1 for "/"
		if nameLen > maxNameLen {
			maxNameLen = nameLen
		}
	}

	// Styles
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Border).
		Padding(0, 1)

	cmdStyle := lipgloss.NewStyle().
		Foreground(theme.Secondary)

	selectedCmdStyle := lipgloss.NewStyle().
		Foreground(theme.Primary).
		Bold(true)

	descStyle := lipgloss.NewStyle().
		Foreground(theme.Muted)

	// Build content
	var b strings.Builder

	// Items with aligned descriptions
	for i, item := range items {
		name := "/" + item.Name
		padding := strings.Repeat(" ", maxNameLen-len(name)+2)

		if i == c.cursor {
			b.WriteString(selectedCmdStyle.Render("❯ " + name))
			b.WriteString(padding)
			b.WriteString(descStyle.Render(item.Description))
		} else {
			b.WriteString("  ")
			b.WriteString(cmdStyle.Render(name))
			b.WriteString(padding)
			b.WriteString(descStyle.Render(item.Description))
		}

		if i < len(items)-1 {
			b.WriteString("\n")
		}
	}

	// Show count if more items
	if len(c.filtered) > maxItems {
		remaining := len(c.filtered) - maxItems
		b.WriteString("\n")
		b.WriteString(descStyle.Render("  ... " + itoa(remaining) + " more"))
	}

	return borderStyle.Render(b.String())
}

// itoa converts int to string without importing strconv
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
