package chat

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/ui"
)

// DialogType represents the type of dialog
type DialogType int

const (
	DialogNone DialogType = iota
	DialogModelPicker
	DialogSessionList
	DialogDirApproval
	DialogMCPPicker
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

// ShowMCPPicker opens the MCP server picker dialog
func (d *DialogModel) ShowMCPPicker(mcpManager *mcp.Manager) {
	d.dialogType = DialogMCPPicker
	d.title = "MCP Servers"
	d.cursor = 0
	d.query = ""
	d.items = nil

	available := mcpManager.AvailableServers()
	states := mcpManager.GetAllStates()

	// Build status map
	statusMap := make(map[string]string)
	for _, state := range states {
		statusMap[state.Name] = string(state.Status)
	}

	for _, name := range available {
		status := statusMap[name]
		if status == "" {
			status = "stopped"
		}
		isRunning := status == "ready" || status == "starting"

		d.items = append(d.items, DialogItem{
			ID:          name,
			Label:       name,
			Description: status,
			Selected:    isRunning,
		})
	}
	d.filtered = d.items
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

// SetQuery updates the filter query for model picker or MCP picker
func (d *DialogModel) SetQuery(query string) {
	d.query = query
	if d.dialogType == DialogModelPicker || d.dialogType == DialogMCPPicker {
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

// Cursor returns the current cursor position
func (d *DialogModel) Cursor() int {
	return d.cursor
}

// SetCursor sets the cursor position, clamping to valid range
func (d *DialogModel) SetCursor(pos int) {
	if pos < 0 {
		pos = 0
	}
	if len(d.filtered) > 0 && pos >= len(d.filtered) {
		pos = len(d.filtered) - 1
	}
	d.cursor = pos
}

// Update handles messages
func (d *DialogModel) Update(msg tea.Msg) (*DialogModel, tea.Cmd) {
	if d.dialogType == DialogNone {
		return d, nil
	}

	// Use filtered list for navigation bounds when filtering is active
	listLen := len(d.items)
	if len(d.filtered) > 0 || d.query != "" {
		listLen = len(d.filtered)
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
			if d.cursor > 0 {
				d.cursor--
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
			if d.cursor < listLen-1 {
				d.cursor++
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q", "ctrl+c"))):
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

	// Use MCP-specific view for MCP picker
	if d.dialogType == DialogMCPPicker {
		return d.viewMCPPicker()
	}

	// Original style for other dialogs (dir approval, session list)
	return d.viewStandardDialog()
}

// viewModelPicker renders completions-style model picker
func (d *DialogModel) viewModelPicker() string {
	theme := d.styles.Theme()

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

	// Show filter input at top
	b.WriteString(mutedStyle.Render("filter: "))
	if d.query != "" {
		b.WriteString(d.query)
	}
	b.WriteString("█") // cursor
	b.WriteString("\n")

	// Handle empty filter results
	if len(d.filtered) == 0 {
		if d.query != "" {
			b.WriteString(mutedStyle.Render("no matches"))
		}
		return borderStyle.Render(b.String())
	}

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

// viewMCPPicker renders the MCP server picker dialog
func (d *DialogModel) viewMCPPicker() string {
	theme := d.styles.Theme()

	dialogWidth := 50
	if dialogWidth > d.width-4 {
		dialogWidth = d.width - 4
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

	successStyle := lipgloss.NewStyle().
		Foreground(theme.Success)

	warningStyle := lipgloss.NewStyle().
		Foreground(theme.Warning)

	errorStyle := lipgloss.NewStyle().
		Foreground(theme.Error)

	var b strings.Builder

	b.WriteString(titleStyle.Render(d.title))
	b.WriteString("\n\n")

	// Handle empty state (no servers configured)
	if len(d.items) == 0 {
		b.WriteString(mutedStyle.Render("No servers configured yet."))
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render("Use /mcp add <name> to add one."))
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("esc close"))
		return borderStyle.Render(b.String())
	}

	// Handle empty filter results
	if len(d.filtered) == 0 && d.query != "" {
		b.WriteString(mutedStyle.Render("No matches for \"" + d.query + "\""))
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("filter: " + d.query + " · backspace clear · esc close"))
		return borderStyle.Render(b.String())
	}

	maxItems := 15
	items := d.filtered
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

	for i, item := range items {
		actualIdx := startIdx + i

		// Status indicator with new icons
		var statusIcon string
		switch item.Description {
		case "ready":
			statusIcon = successStyle.Render("●")
		case "starting":
			statusIcon = warningStyle.Render("◐")
		case "failed":
			statusIcon = errorStyle.Render("○")
		default:
			statusIcon = mutedStyle.Render(" ")
		}

		// Cursor indicator
		cursor := "  "
		if actualIdx == d.cursor {
			cursor = "❯ "
		}

		// Status text
		statusText := ""
		switch item.Description {
		case "ready":
			statusText = successStyle.Render(" ready")
		case "starting":
			statusText = warningStyle.Render(" starting...")
		case "failed":
			statusText = errorStyle.Render(" failed")
		default:
			// No status text for stopped servers - cleaner look
		}

		line := cursor + statusIcon + " " + item.Label + statusText
		if actualIdx == d.cursor {
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString(line)
		}

		if i < len(items)-1 {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n\n")

	// Show filter query if active, otherwise show help text
	if d.query != "" {
		b.WriteString(mutedStyle.Render("filter: " + d.query + " · enter toggle · backspace clear · esc close"))
	} else {
		b.WriteString(mutedStyle.Render("↑↓ navigate · enter toggle · type to filter · esc close"))
	}

	return borderStyle.Render(b.String())
}

// ProviderInfo holds provider and model information
type ProviderInfo struct {
	Name   string
	Models []string
}

// GetAvailableProviders returns providers with their models in consistent order
// If cfg is provided, custom configured providers are also included
func GetAvailableProviders(cfg *config.Config) []ProviderInfo {
	var providers []ProviderInfo
	seen := make(map[string]bool)

	// Add built-in providers first, merging any configured model
	for _, name := range llm.GetBuiltInProviderNames() {
		if models, ok := llm.ProviderModels[name]; ok {
			modelList := make([]string, len(models))
			copy(modelList, models)

			// If config has a model for this provider, prepend it if not already present
			if cfg != nil {
				if providerCfg, ok := cfg.Providers[name]; ok && providerCfg.Model != "" {
					found := false
					for _, m := range modelList {
						if m == providerCfg.Model {
							found = true
							break
						}
					}
					if !found {
						modelList = append([]string{providerCfg.Model}, modelList...)
					}
				}
			}

			providers = append(providers, ProviderInfo{
				Name:   name,
				Models: modelList,
			})
			seen[name] = true
		}
	}

	// Add custom configured providers from config
	if cfg != nil {
		for name, providerCfg := range cfg.Providers {
			if seen[name] {
				continue
			}
			var models []string
			if len(providerCfg.Models) > 0 {
				models = providerCfg.Models
			} else if providerCfg.Model != "" {
				models = []string{providerCfg.Model}
			}
			if len(models) > 0 {
				providers = append(providers, ProviderInfo{
					Name:   name,
					Models: models,
				})
			}
		}
	}

	return providers
}
