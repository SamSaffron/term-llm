package chat

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/ui"
)

// DialogType represents the type of dialog
type DialogType int

const (
	DialogNone DialogType = iota
	DialogModelPicker
	DialogSessionList
	DialogDirApproval
)

// DialogModel handles modal dialogs
type DialogModel struct {
	dialogType DialogType
	items      []DialogItem
	filtered   []DialogItem
	cursor     int
	query      string
	title      string
	width      int
	height     int
	styles     *ui.Styles

	// Directory approval specific
	dirApprovalPath    string
	dirApprovalOptions []string
}

// DialogItem represents an item in a dialog list
type DialogItem struct {
	ID          string
	Label       string
	Description string
	Selected    bool
	Category    string
}

// NewDialogModel creates a new dialog model
func NewDialogModel(styles *ui.Styles) *DialogModel {
	return &DialogModel{
		dialogType: DialogNone,
		styles:     styles,
	}
}

// SetSize updates the dimensions
func (d *DialogModel) SetSize(width, height int) {
	d.width = width
	d.height = height
}

// IsOpen returns whether a dialog is open
func (d *DialogModel) IsOpen() bool {
	return d.dialogType != DialogNone
}

// Type returns the current dialog type
func (d *DialogModel) Type() DialogType {
	return d.dialogType
}

// Close closes the dialog
func (d *DialogModel) Close() {
	d.dialogType = DialogNone
	d.items = nil
	d.cursor = 0
}

// ShowModelPicker opens the model picker dialog
func (d *DialogModel) ShowModelPicker(currentModel string, providers []ProviderInfo) {
	d.dialogType = DialogModelPicker
	d.title = "Select Model"
	d.cursor = 0
	d.query = ""
	d.items = nil

	for _, p := range providers {
		for _, model := range p.Models {
			item := DialogItem{
				ID:       p.Name + ":" + model,
				Label:    p.Name + ":" + model,
				Category: p.Name,
				Selected: model == currentModel,
			}
			d.items = append(d.items, item)
		}
	}
	d.filtered = d.items

	// Find current model and set cursor
	for i, item := range d.filtered {
		if item.Selected {
			d.cursor = i
			break
		}
	}
}

// ShowSessionList opens the session list dialog
func (d *DialogModel) ShowSessionList(sessions []string, currentSession string) {
	d.dialogType = DialogSessionList
	d.title = "Load Session"
	d.cursor = 0
	d.items = nil

	for _, name := range sessions {
		item := DialogItem{
			ID:       name,
			Label:    name,
			Selected: name == currentSession,
		}
		d.items = append(d.items, item)
		if item.Selected {
			d.cursor = len(d.items) - 1
		}
	}
}

// ShowDirApproval opens the directory approval dialog
func (d *DialogModel) ShowDirApproval(filePath string, options []string) {
	d.dialogType = DialogDirApproval
	d.title = "Directory Access"
	d.cursor = 0
	d.items = nil
	d.dirApprovalPath = filePath
	d.dirApprovalOptions = options

	for _, dir := range options {
		d.items = append(d.items, DialogItem{
			ID:          dir,
			Label:       "Allow: " + dir,
			Description: "",
		})
	}

	// Add deny option
	d.items = append(d.items, DialogItem{
		ID:    "__deny__",
		Label: "Deny",
	})
}

// GetDirApprovalPath returns the path that triggered the approval request
func (d *DialogModel) GetDirApprovalPath() string {
	return d.dirApprovalPath
}

// Selected returns the currently highlighted item
func (d *DialogModel) Selected() *DialogItem {
	if len(d.filtered) == 0 {
		return nil
	}
	if d.cursor >= len(d.filtered) {
		d.cursor = len(d.filtered) - 1
	}
	return &d.filtered[d.cursor]
}

// ItemAt returns the item at the given index (0-based)
func (d *DialogModel) ItemAt(idx int) *DialogItem {
	if idx < 0 || idx >= len(d.filtered) {
		return nil
	}
	return &d.filtered[idx]
}

// SetQuery updates the filter query for model picker
func (d *DialogModel) SetQuery(query string) {
	d.query = query
	if d.dialogType == DialogModelPicker {
		d.filterItems()
	}
}

// filterItems filters items based on query
func (d *DialogModel) filterItems() {
	if d.query == "" {
		d.filtered = d.items
	} else {
		d.filtered = nil
		q := strings.ToLower(d.query)
		for _, item := range d.items {
			if strings.Contains(strings.ToLower(item.Label), q) {
				d.filtered = append(d.filtered, item)
			}
		}
	}
	if d.cursor >= len(d.filtered) {
		d.cursor = max(0, len(d.filtered)-1)
	}
}

// Query returns the current filter query
func (d *DialogModel) Query() string {
	return d.query
}

// Update handles messages
func (d *DialogModel) Update(msg tea.Msg) (*DialogModel, tea.Cmd) {
	if d.dialogType == DialogNone {
		return d, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
			if d.cursor > 0 {
				d.cursor--
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
			if d.cursor < len(d.items)-1 {
				d.cursor++
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))):
			d.Close()
		}
	}

	return d, nil
}

// View renders the dialog
func (d *DialogModel) View() string {
	if d.dialogType == DialogNone {
		return ""
	}

	// Use completions-style for model picker
	if d.dialogType == DialogModelPicker {
		return d.viewModelPicker()
	}

	// Original style for other dialogs (dir approval, session list)
	return d.viewStandardDialog()
}

// viewModelPicker renders completions-style model picker
func (d *DialogModel) viewModelPicker() string {
	if len(d.filtered) == 0 {
		return ""
	}

	theme := d.styles.Theme()
	maxVisible := 12

	// Calculate visible window based on cursor position
	startIdx := 0
	if d.cursor >= maxVisible {
		startIdx = d.cursor - maxVisible + 1
	}
	endIdx := startIdx + maxVisible
	if endIdx > len(d.filtered) {
		endIdx = len(d.filtered)
	}
	items := d.filtered[startIdx:endIdx]

	// Styles (matching completions)
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Border).
		Padding(0, 1)

	itemStyle := lipgloss.NewStyle().
		Foreground(theme.Secondary)

	selectedStyle := lipgloss.NewStyle().
		Foreground(theme.Primary).
		Bold(true)

	mutedStyle := lipgloss.NewStyle().
		Foreground(theme.Muted)

	// Build content
	var b strings.Builder

	for i, item := range items {
		actualIdx := startIdx + i
		if actualIdx == d.cursor {
			b.WriteString(selectedStyle.Render("❯ " + item.Label))
		} else {
			b.WriteString("  ")
			b.WriteString(itemStyle.Render(item.Label))
		}

		if item.Selected {
			b.WriteString(mutedStyle.Render(" (current)"))
		}

		if i < len(items)-1 {
			b.WriteString("\n")
		}
	}

	return borderStyle.Render(b.String())
}

// viewStandardDialog renders the standard dialog style
func (d *DialogModel) viewStandardDialog() string {
	theme := d.styles.Theme()

	dialogWidth := 60
	if dialogWidth > d.width-4 {
		dialogWidth = d.width - 4
	}

	maxItems := 15
	items := d.filtered
	if len(items) == 0 {
		items = d.items
	}
	startIdx := 0
	if len(items) > maxItems {
		if d.cursor >= maxItems {
			startIdx = d.cursor - maxItems + 1
		}
		items = items[startIdx:]
		if len(items) > maxItems {
			items = items[:maxItems]
		}
	}

	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Border).
		Padding(1, 2).
		Width(dialogWidth)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(theme.Primary).
		MarginBottom(1)

	selectedStyle := lipgloss.NewStyle().
		Background(theme.Primary).
		Foreground(lipgloss.Color("0"))

	mutedStyle := lipgloss.NewStyle().
		Foreground(theme.Muted)

	var b strings.Builder

	b.WriteString(titleStyle.Render(d.title))
	b.WriteString("\n")

	// Special message for directory approval
	if d.dialogType == DialogDirApproval && d.dirApprovalPath != "" {
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render("Allow access to read files from:"))
		b.WriteString("\n\n")
		b.WriteString("  " + d.dirApprovalPath)
		b.WriteString("\n\n")
	}

	for i, item := range items {
		actualIdx := startIdx + i
		prefix := "  "
		if actualIdx == d.cursor {
			prefix = "❯ "
		}

		label := item.Label
		if actualIdx == d.cursor {
			b.WriteString(selectedStyle.Render(prefix + label))
		} else {
			b.WriteString(prefix + label)
		}

		if i < len(items)-1 {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n\n")
	b.WriteString(mutedStyle.Render("j/k navigate · enter select · esc cancel"))

	return borderStyle.Render(b.String())
}

// ProviderInfo holds provider and model information
type ProviderInfo struct {
	Name   string
	Models []string
}

// ProviderModels contains common models per provider (imported from llm package logic)
var providerModels = map[string][]string{
	"anthropic": {
		"claude-sonnet-4-5",
		"claude-sonnet-4-5-thinking",
		"claude-opus-4-5",
		"claude-opus-4-5-thinking",
		"claude-haiku-4-5",
	},
	"openai": {
		"gpt-5.2",
		"gpt-5.2-high",
		"gpt-5.2-codex",
		"gpt-4.1",
	},
	"gemini": {
		"gemini-3-pro-preview",
		"gemini-3-flash-preview",
		"gemini-2.5-flash",
	},
	"zen": {
		"glm-4.7-free",
		"grok-code",
		"minimax-m2.1-free",
	},
	"ollama": {
		"llama3",
		"codellama",
		"mistral",
	},
}

// GetAvailableProviders returns providers with their models in consistent order
func GetAvailableProviders() []ProviderInfo {
	// Define provider order for consistent results
	providerOrder := []string{"anthropic", "openai", "gemini", "zen", "ollama"}

	var providers []ProviderInfo
	for _, name := range providerOrder {
		if models, ok := providerModels[name]; ok {
			providers = append(providers, ProviderInfo{
				Name:   name,
				Models: models,
			})
		}
	}
	return providers
}
