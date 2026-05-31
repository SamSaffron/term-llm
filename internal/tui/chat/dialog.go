package chat

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	DialogContent
	DialogWorktreePicker
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

	// Worktree picker specific
	worktreeFooter string

	// Static content modal specific
	contentLines  []string
	contentScroll int
	contentFooter string
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
	if styles == nil {
		styles = ui.DefaultStyles()
	}
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
	d.filtered = nil
	d.query = ""
	d.cursor = 0
	d.contentLines = nil
	d.contentScroll = 0
	d.contentFooter = ""
	d.worktreeFooter = ""
}

// ShowModelPicker opens the model picker dialog.
// currentProviderModel is the full "provider:model" identifier of the active model.
// recentModels is an optional MRU-ordered list of "provider:model" strings;
// matching items are floated to the top of the list.
func (d *DialogModel) ShowModelPicker(currentProviderModel string, providers []ProviderInfo, recentModels []string) {
	d.dialogType = DialogModelPicker
	d.title = "Select Model"
	d.cursor = 0
	d.query = ""
	d.items = nil

	for _, p := range providers {
		for _, model := range p.Models {
			id := p.Name + ":" + model
			item := DialogItem{
				ID:       id,
				Label:    id,
				Category: p.Name,
				Selected: id == currentProviderModel,
			}
			d.items = append(d.items, item)
		}
	}

	// Reorder: recent models first, preserving MRU order, then the rest.
	if len(recentModels) > 0 {
		recentRank := make(map[string]int, len(recentModels))
		for i, m := range recentModels {
			recentRank[m] = i
		}
		var recent, rest []DialogItem
		for _, item := range d.items {
			if _, ok := recentRank[item.ID]; ok {
				recent = append(recent, item)
			} else {
				rest = append(rest, item)
			}
		}
		slices.SortFunc(recent, func(a, b DialogItem) int {
			return recentRank[a.ID] - recentRank[b.ID]
		})
		d.items = append(recent, rest...)
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

// ShowSessionList opens the session list dialog.
// items should have ID=full session ID, Label=display name.
func (d *DialogModel) ShowSessionList(items []DialogItem, currentSessionID string) {
	d.dialogType = DialogSessionList
	d.title = "Resume Session"
	d.cursor = 0
	d.query = ""
	d.items = nil
	d.filtered = nil

	for _, item := range items {
		item.Selected = item.ID == currentSessionID
		d.items = append(d.items, item)
		if item.Selected {
			d.cursor = len(d.items) - 1
		}
	}
	d.filtered = d.items
}

// ShowDirApproval opens the directory approval dialog
func (d *DialogModel) ShowDirApproval(filePath string, options []string) {
	d.dialogType = DialogDirApproval
	d.title = "Directory Access"
	d.cursor = 0
	d.query = ""
	d.items = nil
	d.filtered = nil
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
	d.filtered = d.items
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

// ShowWorktreePicker opens the worktree switcher dialog. items is the synthetic
// root row, one row per worktree, and a trailing "+ new worktree…" row; currentID
// marks the bound row (its ID, or "__root__" on the root checkout).
func (d *DialogModel) ShowWorktreePicker(items []DialogItem, currentID string) {
	d.dialogType = DialogWorktreePicker
	d.title = "Worktrees"
	d.cursor = 0
	d.query = ""
	d.items = nil
	d.filtered = nil
	d.worktreeFooter = "enter switch · n new · d delete (×2) · s shell · q close"
	for _, item := range items {
		item.Selected = item.ID == currentID
		d.items = append(d.items, item)
		if item.Selected {
			d.cursor = len(d.items) - 1
		}
	}
	d.filtered = d.items
}

// ShowContent opens a scrollable static content modal.
func (d *DialogModel) ShowContent(title, content string) {
	d.dialogType = DialogContent
	d.title = title
	d.items = nil
	d.filtered = nil
	d.query = ""
	d.cursor = 0
	d.contentScroll = 0
	d.contentFooter = "↑/↓ scroll · pgup/pgdn page · esc close"
	content = strings.TrimRight(content, "\n")
	if content == "" {
		d.contentLines = []string{""}
	} else {
		d.contentLines = strings.Split(content, "\n")
	}
}

// Content returns the raw content currently displayed by a content dialog.
func (d *DialogModel) Content() string {
	return strings.Join(d.contentLines, "\n")
}

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
	case tea.MouseWheelMsg:
		if d.dialogType == DialogContent {
			mouse := msg.Mouse()
			switch mouse.Button {
			case tea.MouseWheelUp:
				d.scrollContentBy(-3)
			case tea.MouseWheelDown:
				d.scrollContentBy(3)
			case tea.MouseWheelLeft, tea.MouseWheelRight:
				// Content dialogs do not scroll horizontally; consume the event so
				// an open modal does not move the underlying viewport.
			}
			return d, nil
		}
	case tea.KeyPressMsg:
		if d.dialogType == DialogContent {
			visible := d.contentVisibleLines()
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
				d.scrollContentBy(-1)
			case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
				d.scrollContentBy(1)
			case key.Matches(msg, key.NewBinding(key.WithKeys("pgup"))):
				d.scrollContentBy(-visible)
			case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown"))):
				d.scrollContentBy(visible)
			case key.Matches(msg, key.NewBinding(key.WithKeys("home"))):
				d.contentScroll = 0
			case key.Matches(msg, key.NewBinding(key.WithKeys("end"))):
				d.contentScroll = d.maxContentScroll()
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q", "ctrl+c"))):
				d.Close()
			}
			return d, nil
		}
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

	// Use content renderer for static modals.
	if d.dialogType == DialogContent {
		return d.viewContentDialog()
	}

	if d.dialogType == DialogWorktreePicker {
		return d.viewWorktreePicker()
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
	startIdx, endIdx := ui.VisibleRange(len(d.filtered), d.cursor, maxVisible)
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

func (d *DialogModel) maxContentScroll() int {
	return max(0, len(d.contentLines)-d.contentVisibleLines())
}

func (d *DialogModel) scrollContentBy(delta int) {
	d.contentScroll = min(d.maxContentScroll(), max(0, d.contentScroll+delta))
}

func (d *DialogModel) contentVisibleLines() int {
	h := d.height - 8
	if h <= 0 || h > 28 {
		h = 28
	}
	if h < 6 {
		h = 6
	}
	return h
}

// viewContentDialog renders a centered-style static content modal.
func (d *DialogModel) viewContentDialog() string {
	theme := d.styles.Theme()
	width := d.width - 4
	if width <= 0 || width > 100 {
		width = 100
	}
	if width < 40 {
		width = 40
	}
	bodyWidth := width - 4
	visible := d.contentVisibleLines()
	maxScroll := max(0, len(d.contentLines)-visible)
	if d.contentScroll > maxScroll {
		d.contentScroll = maxScroll
	}
	end := min(len(d.contentLines), d.contentScroll+visible)
	lines := d.contentLines[d.contentScroll:end]

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Primary)
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	bodyStyle := lipgloss.NewStyle().Width(bodyWidth)
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Border).
		Padding(1, 2).
		Width(width)

	var b strings.Builder
	b.WriteString(titleStyle.Render(d.title))
	b.WriteString("\n\n")
	for i, line := range lines {
		b.WriteString(bodyStyle.Render(line))
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	for i := len(lines); i < visible; i++ {
		b.WriteString("\n")
	}
	b.WriteString("\n\n")
	footer := d.contentFooter
	if maxScroll > 0 {
		footer = fmt.Sprintf("%s · %d/%d", footer, d.contentScroll+1, maxScroll+1)
	}
	b.WriteString(mutedStyle.Render(footer))
	return borderStyle.Render(b.String())
}

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
	startIdx, endIdx := ui.VisibleRange(len(items), d.cursor, maxItems)
	items = items[startIdx:endIdx]

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

// viewWorktreePicker renders the worktree switcher: a status dot (colored by
// lifecycle), a filled/hollow marker for the bound row, the name, and a meta
// column. The footer lists the available keys.
func (d *DialogModel) viewWorktreePicker() string {
	theme := d.styles.Theme()
	width := d.width - 4
	if width <= 0 || width > 86 {
		width = 86
	}
	if width < 48 {
		width = 48
	}
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Border).
		Padding(1, 2).
		Width(width)
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Primary).MarginBottom(1)
	selectedStyle := lipgloss.NewStyle().Background(theme.Primary).Foreground(lipgloss.Color("0"))
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	successStyle := lipgloss.NewStyle().Foreground(theme.Success)
	warningStyle := lipgloss.NewStyle().Foreground(theme.Warning)
	errorStyle := lipgloss.NewStyle().Foreground(theme.Error)

	var b strings.Builder
	b.WriteString(titleStyle.Render(d.title))
	b.WriteString("\n")
	items := d.filtered
	if len(items) == 0 {
		b.WriteString(mutedStyle.Render("No worktrees found."))
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render(d.worktreeFooter))
		return borderStyle.Render(b.String())
	}
	maxItems := 12
	startIdx, endIdx := ui.VisibleRange(len(items), d.cursor, maxItems)
	for i, item := range items[startIdx:endIdx] {
		actualIdx := startIdx + i
		cursor := "  "
		if actualIdx == d.cursor {
			cursor = "❯ "
		}
		statusIcon := successStyle.Render("●")
		switch item.Category {
		case "creating":
			statusIcon = warningStyle.Render("◐")
		case "failed":
			statusIcon = errorStyle.Render("○")
		case "new":
			statusIcon = mutedStyle.Render("+")
		}
		filled := "○"
		if item.Selected {
			filled = "●"
		}
		line := fmt.Sprintf("%s%s %s %-18s %s", cursor, statusIcon, filled, item.Label, item.Description)
		if actualIdx == d.cursor {
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString(line)
		}
		if i < endIdx-startIdx-1 {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n\n")
	b.WriteString(mutedStyle.Render(d.worktreeFooter))
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
	startIdx, endIdx := ui.VisibleRange(len(items), d.cursor, maxItems)
	items = items[startIdx:endIdx]

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
		curated := llm.ProviderModelIDs(name)
		if len(curated) == 0 {
			continue
		}
		var defaultModel string
		if cfg != nil {
			if providerCfg, ok := cfg.Providers[name]; ok {
				defaultModel = providerCfg.Model
			}
		}
		ordered := llm.SortModelIDsByPopularity(name, defaultModel, curated)
		providers = append(providers, ProviderInfo{
			Name:   name,
			Models: llm.ExpandWithEffortVariants(ordered),
		})
		seen[name] = true
	}

	// Add custom configured providers from config
	if cfg != nil {
		for name, providerCfg := range cfg.Providers {
			if seen[name] {
				continue
			}
			source := providerCfg.Models
			if len(source) == 0 {
				source = llm.ResolveProviderModelIDs(name)
			}
			ordered := llm.SortModelIDsByPopularity(name, providerCfg.Model, source)
			if len(ordered) == 0 {
				continue
			}
			providers = append(providers, ProviderInfo{
				Name:   name,
				Models: llm.ExpandWithEffortVariants(ordered),
			})
		}
	}

	return providers
}
