package inspector

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

// CloseMsg signals that the inspector should be closed
type CloseMsg struct{}

// Config holds optional configuration for the inspector
type Config struct {
	ProviderName string
	ModelName    string
	ToolSpecs    []llm.ToolSpec
}

// Model is the conversation inspector model
type Model struct {
	// Dimensions
	width  int
	height int

	// Content
	messages     []session.Message
	contentLines []string // Pre-rendered content split into lines
	totalLines   int

	// Item tracking for truncation/expand
	items         []ContentItem   // All content items (messages, tool calls, tool results)
	expandedItems map[string]bool // IDs of items that should be expanded
	itemAtLine    []int           // line number -> item index (-1 if no item at that line)

	// Scroll state
	scrollY int

	// Components
	styles *ui.Styles
	keyMap KeyMap

	// Session store for fetching subagent messages
	store session.Store

	// Optional configuration
	providerName string
	modelName    string
	toolSpecs    []llm.ToolSpec
}

// New creates a new inspector model
func New(messages []session.Message, width, height int, styles *ui.Styles) *Model {
	return NewWithStore(messages, width, height, styles, nil)
}

// NewWithStore creates a new inspector model with a session store for subagent message fetching
func NewWithStore(messages []session.Message, width, height int, styles *ui.Styles, store session.Store) *Model {
	return NewWithConfig(messages, width, height, styles, store, nil)
}

// NewWithConfig creates a new inspector model with full configuration
func NewWithConfig(messages []session.Message, width, height int, styles *ui.Styles, store session.Store, cfg *Config) *Model {
	if styles == nil {
		styles = ui.DefaultStyles()
	}

	m := &Model{
		width:         width,
		height:        height,
		messages:      messages,
		styles:        styles,
		keyMap:        DefaultKeyMap(),
		expandedItems: make(map[string]bool),
		store:         store,
	}

	// Apply config if provided
	if cfg != nil {
		m.providerName = cfg.ProviderName
		m.modelName = cfg.ModelName
		m.toolSpecs = cfg.ToolSpecs
	}

	m.renderContent()
	return m
}

// renderContent renders all messages and splits into lines
func (m *Model) renderContent() {
	renderer := NewContentRenderer(m.width-2, m.styles, m.expandedItems, m.store, m.providerName, m.modelName, m.toolSpecs) // -2 for padding
	content, items := renderer.RenderMessages(m.messages)
	m.contentLines = strings.Split(content, "\n")
	m.totalLines = len(m.contentLines)
	m.items = items

	// Build line -> item index lookup
	m.itemAtLine = make([]int, m.totalLines)
	for i := range m.itemAtLine {
		m.itemAtLine[i] = -1 // No item at this line by default
	}
	for idx, item := range m.items {
		for line := item.StartLine; line < item.EndLine && line < m.totalLines; line++ {
			m.itemAtLine[line] = idx
		}
	}
}

// Init initializes the model
func (m *Model) Init() tea.Cmd {
	return nil
}

// Update handles messages
func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.renderContent()
		// Adjust scroll if needed
		m.clampScroll()
	}

	return m, nil
}

// handleKeyMsg handles keyboard input
func (m *Model) handleKeyMsg(msg tea.KeyMsg) (*Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keyMap.Quit):
		return m, func() tea.Msg { return CloseMsg{} }

	case key.Matches(msg, m.keyMap.ScrollUp):
		m.scrollY--
		m.clampScroll()

	case key.Matches(msg, m.keyMap.ScrollDown):
		m.scrollY++
		m.clampScroll()

	case key.Matches(msg, m.keyMap.PageUp):
		m.scrollY -= m.viewportHeight()
		m.clampScroll()

	case key.Matches(msg, m.keyMap.PageDown):
		m.scrollY += m.viewportHeight()
		m.clampScroll()

	case key.Matches(msg, m.keyMap.HalfPageUp):
		m.scrollY -= m.viewportHeight() / 2
		m.clampScroll()

	case key.Matches(msg, m.keyMap.HalfPageDown):
		m.scrollY += m.viewportHeight() / 2
		m.clampScroll()

	case key.Matches(msg, m.keyMap.GoToTop):
		m.scrollY = 0

	case key.Matches(msg, m.keyMap.GoToBottom):
		m.scrollY = m.maxScroll()

	case key.Matches(msg, m.keyMap.ExpandVisible):
		m.toggleVisibleItems()
	}

	return m, nil
}

// toggleVisibleItems toggles expand/collapse for items visible in the viewport.
// If any visible items are truncated, expand them. Otherwise, collapse all expanded items.
func (m *Model) toggleVisibleItems() {
	if len(m.items) == 0 || len(m.itemAtLine) == 0 {
		return
	}

	// Find items visible in current viewport
	vpHeight := m.viewportHeight()
	startLine := m.scrollY
	endLine := m.scrollY + vpHeight
	if endLine > m.totalLines {
		endLine = m.totalLines
	}

	// Collect visible items and check their state
	seen := make(map[int]bool)
	var visibleItemIDs []string
	hasCollapsed := false

	for line := startLine; line < endLine; line++ {
		if line >= len(m.itemAtLine) {
			break
		}
		itemIdx := m.itemAtLine[line]
		if itemIdx < 0 || seen[itemIdx] {
			continue
		}
		seen[itemIdx] = true

		item := m.items[itemIdx]
		visibleItemIDs = append(visibleItemIDs, item.ID)
		// Check if this item is truncated (could be expanded)
		if item.IsTruncated && !m.expandedItems[item.ID] {
			hasCollapsed = true
		}
	}

	if len(visibleItemIDs) == 0 {
		return
	}

	changed := false
	if hasCollapsed {
		// Expand all truncated visible items
		for _, id := range visibleItemIDs {
			if !m.expandedItems[id] {
				m.expandedItems[id] = true
				changed = true
			}
		}
	} else {
		// Collapse all expanded visible items
		for _, id := range visibleItemIDs {
			if m.expandedItems[id] {
				delete(m.expandedItems, id)
				changed = true
			}
		}
	}

	// Re-render if we changed anything
	if changed {
		oldScrollY := m.scrollY
		m.renderContent()
		m.scrollY = oldScrollY
		m.clampScroll()
	}
}

// viewportHeight returns the available height for content
func (m *Model) viewportHeight() int {
	// Reserve 3 lines for header and footer
	// Clamp to at least 1 to avoid invalid slice bounds on very small terminals
	return max(1, m.height-3)
}

// maxScroll returns the maximum scroll position
func (m *Model) maxScroll() int {
	max := m.totalLines - m.viewportHeight()
	if max < 0 {
		return 0
	}
	return max
}

// clampScroll ensures scroll is within bounds
func (m *Model) clampScroll() {
	if m.scrollY < 0 {
		m.scrollY = 0
	}
	max := m.maxScroll()
	if m.scrollY > max {
		m.scrollY = max
	}
}

// View renders the model
func (m *Model) View() string {
	theme := m.styles.Theme()
	var b strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(theme.Text).
		Background(theme.Border).
		Padding(0, 1).
		Width(m.width)

	title := "Conversation Inspector"
	msgCount := len(m.messages)
	if msgCount == 1 {
		title += " (1 message)"
	} else {
		title += fmt.Sprintf(" (%d messages)", msgCount)
	}

	b.WriteString(headerStyle.Render(title))
	b.WriteString("\n")

	// Content viewport
	vpHeight := m.viewportHeight()
	endIdx := m.scrollY + vpHeight
	if endIdx > m.totalLines {
		endIdx = m.totalLines
	}

	// Defensive bounds checking for very small terminals or empty content
	startIdx := m.scrollY
	if startIdx > m.totalLines {
		startIdx = m.totalLines
	}
	if startIdx > endIdx {
		startIdx = endIdx
	}

	visibleLines := m.contentLines[startIdx:endIdx]
	content := strings.Join(visibleLines, "\n")

	// Pad content to fill viewport
	lineCount := len(visibleLines)
	if lineCount < vpHeight {
		content += strings.Repeat("\n", vpHeight-lineCount)
	}

	b.WriteString(content)
	b.WriteString("\n")

	// Footer with scroll info and help
	// Note: We don't use lipgloss Width() style here because we manually
	// pad to avoid issues with ANSI escape codes and double-width handling

	// Scroll indicator
	scrollInfo := ""
	if m.totalLines > vpHeight {
		pct := 0
		if m.maxScroll() > 0 {
			pct = (m.scrollY * 100) / m.maxScroll()
		}
		scrollInfo = fmt.Sprintf("%d-%d/%d (%d%%)", m.scrollY+1, endIdx, m.totalLines, pct)
	}

	// Help text (plain, no styling that could interfere with width calc)
	help := "q:close  j/k:scroll  g/G:top/bottom  e:toggle"

	// Combine footer with manual padding
	padding := m.width - len(scrollInfo) - len(help)
	if padding < 1 {
		padding = 1
	}
	footer := scrollInfo + strings.Repeat(" ", padding) + help

	// Apply muted color to entire footer
	footerStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	b.WriteString(footerStyle.Render(footer))

	return b.String()
}
