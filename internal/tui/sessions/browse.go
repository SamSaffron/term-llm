package sessions

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	"github.com/samsaffron/term-llm/internal/ui"
)

// SortOrder defines how sessions are sorted
type SortOrder int

const (
	SortNewest SortOrder = iota
	SortOldest
	SortStatus
	SortModel
)

func (s SortOrder) String() string {
	switch s {
	case SortNewest:
		return "newest"
	case SortOldest:
		return "oldest"
	case SortStatus:
		return "status"
	case SortModel:
		return "model"
	default:
		return "newest"
	}
}

// StatusFilter defines which session statuses to show
type StatusFilter int

const (
	StatusAll StatusFilter = iota
	StatusActive
	StatusComplete
	StatusError
)

func (s StatusFilter) String() string {
	switch s {
	case StatusAll:
		return "all"
	case StatusActive:
		return "active"
	case StatusComplete:
		return "complete"
	case StatusError:
		return "error"
	default:
		return "all"
	}
}

// InspectMsg signals that a session should be inspected
type InspectMsg struct {
	SessionID string
}

// ChatMsg signals that a session should be opened for chat
type ChatMsg struct {
	SessionID string
}

// DeleteConfirmMsg signals a delete was confirmed
type DeleteConfirmMsg struct {
	SessionID string
}

// RefreshMsg signals the list should be refreshed
type RefreshMsg struct{}

// Model is the sessions browser model
type Model struct {
	// Dimensions
	width  int
	height int

	// Data
	store    session.Store
	sessions []session.SessionSummary

	// Selection
	cursor int

	// Search/filter state
	searchInput textinput.Model
	searching   bool
	ftsEnabled  bool
	searchQuery string

	// Sort/filter
	sortOrder    SortOrder
	statusFilter StatusFilter

	// Delete confirmation
	deleteConfirm bool
	deleteID      string
	deleteNumber  int64

	// Inspector state
	inspecting bool
	inspector  *inspector.Model

	// Chat request (set when user wants to chat with a session)
	chatSessionID string

	// Components
	styles *ui.Styles
	keyMap KeyMap

	// Error state
	err error
}

// New creates a new sessions browser model
func New(store session.Store, width, height int, styles *ui.Styles) *Model {
	if styles == nil {
		styles = ui.DefaultStyles()
	}

	ti := textinput.New()
	ti.Placeholder = "Search..."
	ti.CharLimit = 100
	ti.Width = 30

	m := &Model{
		width:       width,
		height:      height,
		store:       store,
		searchInput: ti,
		styles:      styles,
		keyMap:      DefaultKeyMap(),
	}

	return m
}

// ChatSessionID returns the session ID the user wants to chat with, if any.
// Check this after the TUI exits to determine if chat should be launched.
func (m *Model) ChatSessionID() string {
	return m.chatSessionID
}

// Init initializes the model
func (m *Model) Init() tea.Cmd {
	return m.loadSessions
}

// loadSessions fetches sessions from the store
func (m *Model) loadSessions() tea.Msg {
	return RefreshMsg{}
}

// Update handles messages
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// If inspecting, delegate to inspector
	if m.inspecting && m.inspector != nil {
		switch msg := msg.(type) {
		case inspector.CloseMsg:
			m.inspecting = false
			m.inspector = nil
			return m, nil
		case tea.WindowSizeMsg:
			m.width = msg.Width
			m.height = msg.Height
			var cmd tea.Cmd
			m.inspector, cmd = m.inspector.Update(msg)
			return m, cmd
		case tea.KeyMsg:
			var cmd tea.Cmd
			m.inspector, cmd = m.inspector.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case RefreshMsg:
		return m.doRefresh()

	case InspectMsg:
		return m.openInspector(msg.SessionID)

	case ChatMsg:
		m.chatSessionID = msg.SessionID
		return m, tea.Quit

	case DeleteConfirmMsg:
		return m.doDelete(msg.SessionID)
	}

	return m, nil
}

// handleKeyMsg handles keyboard input
func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// If searching, handle search input
	if m.searching {
		switch msg.String() {
		case "enter":
			m.searching = false
			m.searchQuery = m.searchInput.Value()
			return m.doRefresh()
		case "esc":
			m.searching = false
			m.searchInput.SetValue(m.searchQuery) // Restore previous
			return m, nil
		}

		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}

	// Delete confirmation
	if m.deleteConfirm {
		switch msg.String() {
		case "y", "Y":
			m.deleteConfirm = false
			return m, func() tea.Msg { return DeleteConfirmMsg{SessionID: m.deleteID} }
		case "n", "N", "esc":
			m.deleteConfirm = false
			m.deleteID = ""
			return m, nil
		}
		return m, nil
	}

	// Normal key handling
	switch {
	case key.Matches(msg, m.keyMap.Quit):
		return m, tea.Quit

	case key.Matches(msg, m.keyMap.Up):
		m.moveCursor(-1)

	case key.Matches(msg, m.keyMap.Down):
		m.moveCursor(1)

	case key.Matches(msg, m.keyMap.PageUp):
		m.moveCursor(-m.viewportHeight())

	case key.Matches(msg, m.keyMap.PageDown):
		m.moveCursor(m.viewportHeight())

	case key.Matches(msg, m.keyMap.GoToTop):
		m.cursor = 0

	case key.Matches(msg, m.keyMap.GoToBottom):
		if len(m.sessions) > 0 {
			m.cursor = len(m.sessions) - 1
		}

	case key.Matches(msg, m.keyMap.Select):
		if len(m.sessions) > 0 && m.cursor < len(m.sessions) {
			return m, func() tea.Msg { return ChatMsg{SessionID: m.sessions[m.cursor].ID} }
		}

	case key.Matches(msg, m.keyMap.Inspect):
		if len(m.sessions) > 0 && m.cursor < len(m.sessions) {
			return m, func() tea.Msg { return InspectMsg{SessionID: m.sessions[m.cursor].ID} }
		}

	case key.Matches(msg, m.keyMap.Delete):
		if len(m.sessions) > 0 && m.cursor < len(m.sessions) {
			m.deleteConfirm = true
			m.deleteID = m.sessions[m.cursor].ID
			m.deleteNumber = m.sessions[m.cursor].Number
		}

	case key.Matches(msg, m.keyMap.Search):
		m.searching = true
		m.searchInput.Focus()

	case key.Matches(msg, m.keyMap.Sort):
		m.sortOrder = (m.sortOrder + 1) % 4
		return m.doRefresh()

	case key.Matches(msg, m.keyMap.Filter):
		m.statusFilter = (m.statusFilter + 1) % 4
		return m.doRefresh()

	case key.Matches(msg, m.keyMap.ToggleFTS):
		m.ftsEnabled = !m.ftsEnabled
		// Refresh results if there's an active search query
		if m.searchQuery != "" {
			return m.doRefresh()
		}
	}

	return m, nil
}

// moveCursor moves the cursor by delta, clamping to bounds
func (m *Model) moveCursor(delta int) {
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.sessions) && len(m.sessions) > 0 {
		m.cursor = len(m.sessions) - 1
	}
}

// viewportHeight returns the number of visible session rows
func (m *Model) viewportHeight() int {
	// Header (2) + footer (2) + filter bar (1) = 5 lines reserved
	return max(1, m.height-5)
}

// doRefresh fetches sessions from the store
func (m *Model) doRefresh() (tea.Model, tea.Cmd) {
	ctx := context.Background()

	// Clear any previous error
	m.err = nil

	// Map status filter to session status
	var status session.SessionStatus
	switch m.statusFilter {
	case StatusActive:
		status = session.StatusActive
	case StatusComplete:
		status = session.StatusComplete
	case StatusError:
		status = session.StatusError
	}

	// If FTS search is enabled and there's a query, use Search
	if m.ftsEnabled && m.searchQuery != "" {
		results, err := m.store.Search(ctx, m.searchQuery, 100)
		if err != nil {
			m.err = err
			return m, nil
		}
		// Convert search results to summaries (need to fetch each session)
		var summaries []session.SessionSummary
		seen := make(map[string]bool)
		for _, r := range results {
			if seen[r.SessionID] {
				continue
			}
			seen[r.SessionID] = true
			// Fetch full session to get summary data
			sess, err := m.store.Get(ctx, r.SessionID)
			if err != nil || sess == nil {
				continue
			}
			// Apply status filter
			if status != "" && sess.Status != status {
				continue
			}
			msgs, _ := m.store.GetMessages(ctx, sess.ID, 0, 0)
			summaries = append(summaries, session.SessionSummary{
				ID:           sess.ID,
				Number:       sess.Number,
				Name:         sess.Name,
				Summary:      sess.Summary,
				Provider:     sess.Provider,
				Model:        sess.Model,
				Mode:         sess.Mode,
				MessageCount: len(msgs),
				Status:       sess.Status,
				CreatedAt:    sess.CreatedAt,
				UpdatedAt:    sess.UpdatedAt,
			})
		}
		m.sessions = summaries
	} else {
		// Use List with filtering
		summaries, err := m.store.List(ctx, session.ListOptions{
			Status: status,
			Limit:  100,
		})
		if err != nil {
			m.err = err
			return m, nil
		}
		// Filter by search query (simple substring match on summary/name)
		if m.searchQuery != "" {
			query := strings.ToLower(m.searchQuery)
			var filtered []session.SessionSummary
			for _, s := range summaries {
				if strings.Contains(strings.ToLower(s.Summary), query) ||
					strings.Contains(strings.ToLower(s.Name), query) {
					filtered = append(filtered, s)
				}
			}
			summaries = filtered
		}
		m.sessions = summaries
	}

	// Apply sort order
	m.sortSessions()

	// Clamp cursor
	if m.cursor >= len(m.sessions) && len(m.sessions) > 0 {
		m.cursor = len(m.sessions) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}

	return m, nil
}

// sortSessions sorts the sessions list based on current sort order
func (m *Model) sortSessions() {
	switch m.sortOrder {
	case SortNewest:
		sort.Slice(m.sessions, func(i, j int) bool {
			return m.sessions[i].UpdatedAt.After(m.sessions[j].UpdatedAt)
		})
	case SortOldest:
		sort.Slice(m.sessions, func(i, j int) bool {
			return m.sessions[i].UpdatedAt.Before(m.sessions[j].UpdatedAt)
		})
	case SortStatus:
		sort.Slice(m.sessions, func(i, j int) bool {
			return m.sessions[i].Status < m.sessions[j].Status
		})
	case SortModel:
		sort.Slice(m.sessions, func(i, j int) bool {
			return m.sessions[i].Model < m.sessions[j].Model
		})
	}
}

// openInspector opens the session inspector
func (m *Model) openInspector(sessionID string) (tea.Model, tea.Cmd) {
	ctx := context.Background()
	messages, err := m.store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		m.err = err
		return m, nil
	}

	m.inspecting = true
	m.inspector = inspector.NewWithStore(messages, m.width, m.height, m.styles, m.store)
	return m, nil
}

// doDelete deletes a session
func (m *Model) doDelete(sessionID string) (tea.Model, tea.Cmd) {
	ctx := context.Background()
	if err := m.store.Delete(ctx, sessionID); err != nil {
		m.err = err
		return m, nil
	}
	return m.doRefresh()
}

// View renders the model
func (m *Model) View() string {
	// If inspecting, show inspector
	if m.inspecting && m.inspector != nil {
		return m.inspector.View()
	}

	renderWidth := m.width
	if renderWidth < 1 {
		renderWidth = 1
	}

	theme := m.styles.Theme()
	var b strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(theme.Text).
		Background(theme.Border).
		Padding(0, 1).
		Width(renderWidth)

	countStr := fmt.Sprintf("[%d sessions]", len(m.sessions))
	title := "Sessions Browser"
	padding := renderWidth - lipgloss.Width(title) - lipgloss.Width(countStr) - 4 // 4 for padding spaces
	if padding < 1 {
		padding = 1
	}
	header := title + strings.Repeat(" ", padding) + countStr
	b.WriteString(headerStyle.Render(header))
	b.WriteString("\n")

	// Filter bar
	filterStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	var filterParts []string

	// Search box
	if m.searching {
		filterParts = append(filterParts, "[Filter: "+m.searchInput.View()+"]")
	} else if m.searchQuery != "" {
		filterParts = append(filterParts, "[Filter: "+m.searchQuery+"]")
	} else {
		filterParts = append(filterParts, "[Filter: _]")
	}

	filterParts = append(filterParts, fmt.Sprintf("[Sort: %s]", m.sortOrder))
	filterParts = append(filterParts, fmt.Sprintf("[Status: %s]", m.statusFilter))
	if m.ftsEnabled {
		filterParts = append(filterParts, "[FTS: on]")
	}

	b.WriteString(filterStyle.Render(strings.Join(filterParts, " ")))
	b.WriteString("\n")

	// Session list
	vpHeight := m.viewportHeight()

	// Calculate visible range with cursor in view
	start := 0
	if m.cursor >= vpHeight {
		start = m.cursor - vpHeight + 1
	}
	end := start + vpHeight
	if end > len(m.sessions) {
		end = len(m.sessions)
	}

	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(theme.Text).
		Background(theme.Primary)
	normalStyle := lipgloss.NewStyle().Foreground(theme.Text)
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)

	for i := start; i < end; i++ {
		s := m.sessions[i]

		// Format row
		summary := s.Summary
		if s.Name != "" {
			summary = s.Name
		}
		summary = truncateDisplay(summary, 25, "...")

		// Status
		status := string(s.Status)
		if status == "" {
			status = "active"
		}

		// Mode indicator (style: chat/ask)
		style := string(s.Mode)
		if style == "" {
			style = "chat"
		}

		// Tokens
		tokens := formatTokens(s.InputTokens, s.OutputTokens)

		// Age
		age := formatRelativeTime(s.UpdatedAt)

		// Format: cursor # summary style model msgs tokens status age
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}

		// Build row
		row := fmt.Sprintf("%s%4d %-25s %-4s %-10s %3d %-11s %-8s %s",
			cursor, s.Number, summary, style, truncateModel(s.Model, 10), s.MessageCount, tokens, status, age)

		// Truncate or pad to width
		row = fitToDisplayWidth(row, renderWidth)

		if i == m.cursor {
			b.WriteString(selectedStyle.Render(row))
		} else {
			b.WriteString(normalStyle.Render(row))
		}
		b.WriteString("\n")
	}

	// Pad remaining rows
	rendered := end - start
	for i := rendered; i < vpHeight; i++ {
		b.WriteString(strings.Repeat(" ", renderWidth) + "\n")
	}

	// Footer
	b.WriteString(strings.Repeat("─", renderWidth))
	b.WriteString("\n")

	// Delete confirmation
	if m.deleteConfirm {
		confirmStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Error)
		b.WriteString(confirmStyle.Render(fmt.Sprintf("Delete session #%d? (y/n)", m.deleteNumber)))
	} else if m.err != nil {
		errorStyle := lipgloss.NewStyle().Foreground(theme.Error)
		b.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
	} else {
		// Help
		help := "[enter] chat  [i] inspect  [d] delete  [/] search  [s] sort  [f] filter  [q] quit"
		b.WriteString(mutedStyle.Render(help))
	}

	return b.String()
}

// formatRelativeTime returns a human-readable relative time string
func formatRelativeTime(t time.Time) string {
	dur := time.Since(t)
	switch {
	case dur < time.Minute:
		return "now"
	case dur < time.Hour:
		return fmt.Sprintf("%dm", int(dur.Minutes()))
	case dur < 24*time.Hour:
		return fmt.Sprintf("%dh", int(dur.Hours()))
	case dur < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(dur.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// truncateModel shortens a model name to fit
func truncateModel(model string, maxLen int) string {
	return truncateDisplay(model, maxLen, "..")
}

// truncateDisplay truncates a string to maxWidth display cells and adds suffix.
// It preserves both UTF-8 boundaries and display-width boundaries.
func truncateDisplay(s string, maxWidth int, suffix string) string {
	if maxWidth <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= maxWidth {
		return s
	}

	suffixWidth := lipgloss.Width(suffix)
	if suffixWidth >= maxWidth {
		// Keep tiny widths stable while still indicating truncation.
		return strings.Repeat(".", maxWidth)
	}

	targetWidth := maxWidth - suffixWidth
	var b strings.Builder
	current := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if current+rw > targetWidth {
			break
		}
		b.WriteRune(r)
		current += rw
	}
	return b.String() + suffix
}

func fitToDisplayWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	current := lipgloss.Width(s)
	if current > width {
		return truncateDisplay(s, width, "...")
	}
	if current < width {
		return s + strings.Repeat(" ", width-current)
	}
	return s
}

// formatTokens formats input/output tokens in compact form (e.g., "1k/2k")
func formatTokens(input, output int) string {
	if input == 0 && output == 0 {
		return "-"
	}
	return fmt.Sprintf("%s/%s", formatCount(input), formatCount(output))
}

// formatCount formats a number in compact form (e.g., 1k, 1.2k, 3.4M)
func formatCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		val := float64(n) / 1000
		if val == float64(int(val)) {
			return fmt.Sprintf("%dk", int(val))
		}
		return fmt.Sprintf("%.1fk", val)
	}
	val := float64(n) / 1000000
	if val == float64(int(val)) {
		return fmt.Sprintf("%dM", int(val))
	}
	return fmt.Sprintf("%.1fM", val)
}
