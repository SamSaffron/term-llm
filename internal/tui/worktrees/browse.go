package worktrees

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/samsaffron/term-llm/internal/worktree"
)

// CloseMsg asks the parent to close the embedded browser.
type CloseMsg struct{}

// OpenMsg asks the parent to bind the current chat to Worktree.
type OpenMsg struct{ Worktree worktree.Worktree }

// CreateMsg asks the parent to create and bind a worktree.
type CreateMsg struct{ Options worktree.CreateOptions }

// RemoveMsg asks the parent to remove Worktree. Force is true only after the
// user accepts the browser's explicit second warning.
type RemoveMsg struct {
	Worktree worktree.Worktree
	Force    bool
}

// RefreshMsg requests a fresh managed-worktree list.
type RefreshMsg struct{}

type listResultMsg struct {
	items []worktree.Worktree
	err   error
}
type diffResultMsg struct {
	dir        string
	generation uint64
	diff       string
	err        error
}
type inUseResultMsg struct {
	dir        string
	generation uint64
	sessions   []worktree.InUseSession
	err        error
}

type viewMode int

const (
	modeBrowse viewMode = iota
	modeCreate
	modeDetails
	modeDelete
	modeForceDelete
)

// Model is a full-screen embedded managed-worktree browser.
type Model struct {
	root, boundDir string
	store          session.Store
	width, height  int
	styles         *ui.Styles
	items          []worktree.Worktree
	cursor         int
	mode           viewMode
	busy           bool
	refreshing     bool
	status         string
	err            error

	inputs           [3]textinput.Model
	inputFocus       int
	detail           *worktree.Worktree
	detailDiff       string
	detailUsers      []worktree.InUseSession
	diffErr          error
	usersErr         error
	detailBusy       int
	detailScroll     int
	detailGeneration uint64
	deleteTarget     *worktree.Worktree
	forceRisks       []string
}

// New constructs an embedded worktree browser.
func New(root string, store session.Store, boundDir string, width, height int, styles *ui.Styles) *Model {
	if styles == nil {
		styles = ui.DefaultStyles()
	}
	m := &Model{root: root, store: store, boundDir: boundDir, width: width, height: height, styles: styles}
	m.inputs[0] = textinput.New()
	m.inputs[0].Placeholder = "name (required)"
	m.inputs[1] = textinput.New()
	m.inputs[1].Placeholder = "base"
	m.inputs[1].SetValue("HEAD")
	m.inputs[2] = textinput.New()
	m.inputs[2].Placeholder = "branch (optional)"
	for i := range m.inputs {
		m.inputs[i].CharLimit = 200
	}
	return m
}

func (m *Model) Init() tea.Cmd { return m.Refresh() }

// Refresh asynchronously reloads the managed worktrees.
func (m *Model) Refresh() tea.Cmd {
	m.refreshing = true
	m.status = "Refreshing…"
	root := m.root
	return func() tea.Msg {
		items, err := worktree.List(root)
		return listResultMsg{items: items, err: err}
	}
}

// SetItems replaces rows while preserving the selected directory when possible.
// It is also useful for deterministic parent and package tests.
func (m *Model) SetItems(items []worktree.Worktree) {
	selected := ""
	if wt := m.selected(); wt != nil {
		selected = wt.Dir
	}
	m.items = append([]worktree.Worktree(nil), items...)
	if selected != "" {
		for i := range m.items {
			if samePath(m.items[i].Dir, selected) {
				m.cursor = i
				break
			}
		}
	} else if m.boundDir != "" {
		for i := range m.items {
			if samePath(m.items[i].Dir, m.boundDir) {
				m.cursor = i
				break
			}
		}
	}
	m.clampCursor()
}

// SetBusy updates the browser-owned operation indicator.
func (m *Model) SetBusy(busy bool, status string) {
	m.busy, m.status = busy, status
	if busy {
		m.err = nil
	}
}

// ReportCreateResult keeps a failed create visible in its form.
func (m *Model) ReportCreateResult(err error) {
	m.busy = false
	if err != nil {
		m.mode = modeCreate
		m.err = err
	}
}

// ReportRemoveResult reports a completed removal. On success it refreshes; a
// blocked normal removal should instead use EscalateRemove.
func (m *Model) ReportRemoveResult(err error) tea.Cmd {
	m.busy = false
	if err != nil {
		m.err = err
		m.mode = modeBrowse
		m.deleteTarget = nil
		return nil
	}
	m.mode, m.err = modeBrowse, nil
	m.deleteTarget = nil
	m.status = "Worktree removed."
	return m.Refresh()
}

// EscalateRemove presents the required second, explicit force confirmation.
func (m *Model) EscalateRemove(risks []string) {
	m.busy = false
	if m.deleteTarget == nil {
		if wt := m.selected(); wt != nil {
			target := *wt
			m.deleteTarget = &target
		}
	}
	m.mode = modeForceDelete
	m.forceRisks = append([]string(nil), risks...)
	m.err = nil
}

// Error displays an action error without closing the browser.
func (m *Model) Error(err error) { m.busy, m.err = false, err }

// Items returns a copy of the current rows.
func (m *Model) Items() []worktree.Worktree { return append([]worktree.Worktree(nil), m.items...) }
func (m *Model) Cursor() int                { return m.cursor }
func (m *Model) Width() int                 { return m.width }
func (m *Model) Height() int                { return m.height }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case RefreshMsg:
		return m, m.Refresh()
	case listResultMsg:
		m.refreshing = false
		if msg.err != nil {
			m.err = msg.err
			if !m.busy {
				m.status = ""
			}
			return m, nil
		}
		m.SetItems(msg.items)
		m.err = nil
		if !m.busy {
			m.status = ""
		}
		return m, nil
	case diffResultMsg:
		if m.detail != nil && msg.generation == m.detailGeneration && samePath(m.detail.Dir, msg.dir) {
			m.detailDiff, m.diffErr = msg.diff, msg.err
			m.detailBusy--
		}
		return m, nil
	case inUseResultMsg:
		if m.detail != nil && msg.generation == m.detailGeneration && samePath(m.detail.Dir, msg.dir) {
			m.detailUsers, m.usersErr = msg.sessions, msg.err
			m.detailBusy--
		}
		return m, nil
	case tea.PasteMsg:
		if m.mode == modeCreate && !m.busy {
			var cmd tea.Cmd
			m.inputs[m.inputFocus], cmd = m.inputs[m.inputFocus].Update(msg)
			return m, cmd
		}
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.busy {
		if key == "esc" || key == "q" {
			m.err = fmt.Errorf("an operation is still running")
		}
		return m, nil
	}
	switch m.mode {
	case modeCreate:
		return m.handleCreateKey(msg)
	case modeDetails:
		switch key {
		case "esc", "q":
			m.mode, m.detail = modeBrowse, nil
		case "up", "k":
			if m.detailScroll > 0 {
				m.detailScroll--
			}
		case "down", "j":
			m.detailScroll = min(m.detailMaxScroll(), m.detailScroll+1)
		case "pgup":
			m.detailScroll = max(0, m.detailScroll-m.detailHeight())
		case "pgdown":
			m.detailScroll = min(m.detailMaxScroll(), m.detailScroll+m.detailHeight())
		case "g", "home":
			m.detailScroll = 0
		case "G", "end":
			m.detailScroll = m.detailMaxScroll()
		}
		return m, nil
	case modeDelete:
		switch key {
		case "y", "Y":
			m.mode = modeBrowse
			if m.deleteTarget != nil {
				target := *m.deleteTarget
				return m, func() tea.Msg { return RemoveMsg{Worktree: target} }
			}
		case "n", "N", "esc", "q":
			m.mode = modeBrowse
			m.deleteTarget = nil
		}
		return m, nil
	case modeForceDelete:
		switch key {
		case "y", "Y":
			m.mode = modeBrowse
			if m.deleteTarget != nil {
				target := *m.deleteTarget
				return m, func() tea.Msg { return RemoveMsg{Worktree: target, Force: true} }
			}
		case "n", "N", "esc", "q":
			m.mode = modeBrowse
			m.deleteTarget = nil
		}
		return m, nil
	}

	switch key {
	case "esc", "q":
		return m, func() tea.Msg { return CloseMsg{} }
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-m.viewportHeight())
	case "pgdown":
		m.move(m.viewportHeight())
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		if len(m.items) > 0 {
			m.cursor = len(m.items) - 1
		}
	case "enter":
		if wt := m.selected(); wt != nil {
			copy := *wt
			return m, func() tea.Msg { return OpenMsg{Worktree: copy} }
		}
	case "n":
		m.openCreate()
		return m, m.inputs[0].Focus()
	case "i":
		return m.openDetails()
	case "d":
		if wt := m.selected(); wt != nil {
			target := *wt
			m.deleteTarget = &target
			m.mode = modeDelete
			m.err = nil
		}
	case "r":
		return m, m.Refresh()
	}
	return m, nil
}

func (m *Model) openCreate() {
	m.mode, m.inputFocus, m.err = modeCreate, 0, nil
	m.inputs[0].SetValue("")
	m.inputs[1].SetValue("HEAD")
	m.inputs[2].SetValue("")
	for i := range m.inputs {
		if i == 0 {
			m.inputs[i].Focus()
		} else {
			m.inputs[i].Blur()
		}
	}
}

func (m *Model) handleCreateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode, m.err = modeBrowse, nil
		return m, nil
	case "tab", "down":
		m.focusInput(1)
		return m, m.inputs[m.inputFocus].Focus()
	case "shift+tab", "up":
		m.focusInput(-1)
		return m, m.inputs[m.inputFocus].Focus()
	case "enter":
		name := strings.TrimSpace(m.inputs[0].Value())
		base := strings.TrimSpace(m.inputs[1].Value())
		if name == "" {
			m.err = fmt.Errorf("name is required")
			return m, nil
		}
		if base == "" {
			base = "HEAD"
		}
		opts := worktree.CreateOptions{Name: name, Base: base, Branch: strings.TrimSpace(m.inputs[2].Value())}
		m.err = nil
		return m, func() tea.Msg { return CreateMsg{Options: opts} }
	}
	var cmd tea.Cmd
	m.inputs[m.inputFocus], cmd = m.inputs[m.inputFocus].Update(msg)
	return m, cmd
}

func (m *Model) focusInput(delta int) {
	m.inputs[m.inputFocus].Blur()
	m.inputFocus = (m.inputFocus + delta + len(m.inputs)) % len(m.inputs)
}

func (m *Model) openDetails() (tea.Model, tea.Cmd) {
	wt := m.selected()
	if wt == nil {
		return m, nil
	}
	target := *wt
	m.mode, m.detail, m.detailScroll = modeDetails, &target, 0
	m.detailGeneration++
	generation := m.detailGeneration
	m.detailDiff, m.detailUsers, m.diffErr, m.usersErr = "", nil, nil, nil
	m.detailBusy = 2
	dir, store := target.Dir, m.store
	return m, tea.Batch(
		func() tea.Msg {
			diff, err := worktree.Diff(dir)
			return diffResultMsg{dir: dir, generation: generation, diff: diff, err: err}
		},
		func() tea.Msg {
			users, err := worktree.InUse(context.Background(), store, dir)
			return inUseResultMsg{dir: dir, generation: generation, sessions: users, err: err}
		},
	)
}

func (m *Model) selected() *worktree.Worktree {
	if m.cursor < 0 || m.cursor >= len(m.items) {
		return nil
	}
	return &m.items[m.cursor]
}
func (m *Model) move(delta int) { m.cursor += delta; m.clampCursor() }
func (m *Model) clampCursor() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if len(m.items) == 0 {
		m.cursor = 0
	} else if m.cursor >= len(m.items) {
		m.cursor = len(m.items) - 1
	}
}
func (m *Model) viewportHeight() int { return ui.RemainingLines(m.height, 4) }
func (m *Model) detailHeight() int   { return ui.RemainingLines(m.height, 3) }
func (m *Model) detailMaxScroll() int {
	return max(0, len(m.detailLines())-m.detailHeight())
}

func (m *Model) View() tea.View {
	width := max(1, m.width)
	theme := m.styles.Theme()
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Text).Background(theme.Border).Padding(0, 1).Width(width)
	muted := lipgloss.NewStyle().Foreground(theme.Muted)
	errStyle := lipgloss.NewStyle().Foreground(theme.Error)
	selected := lipgloss.NewStyle().Bold(true).Foreground(theme.Text).Background(theme.Primary)
	var b strings.Builder
	title := "Worktree Browser"
	if m.mode == modeCreate {
		title = "Create Worktree"
	} else if m.mode == modeDetails {
		title = "Worktree Details"
	}
	b.WriteString(headerStyle.Render(fit(title, width)))
	b.WriteByte('\n')

	if m.mode == modeCreate {
		labels := []string{"Name", "Base", "Branch"}
		for i := range m.inputs {
			prefix := "  "
			if i == m.inputFocus {
				prefix = "> "
			}
			b.WriteString(fit(fmt.Sprintf("%s%-7s %s", prefix, labels[i]+":", m.inputs[i].View()), width))
			b.WriteByte('\n')
		}
		b.WriteString("\n")
		b.WriteString(muted.Render(fit("Tab/↑/↓ fields  Enter create  Esc cancel", width)))
		if m.busy && m.status != "" {
			b.WriteByte('\n')
			b.WriteString(muted.Render(fit(m.status, width)))
		} else if m.err != nil {
			b.WriteByte('\n')
			b.WriteString(errStyle.Render(fit("Error: "+m.err.Error(), width)))
		}
		return ui.NewAltScreenView(b.String())
	}
	if m.mode == modeDetails {
		lines := m.detailLines()
		h := m.detailHeight()
		scroll := min(m.detailScroll, max(0, len(lines)-h))
		end := min(len(lines), scroll+h)
		for _, line := range lines[scroll:end] {
			b.WriteString(fit(line, width))
			b.WriteByte('\n')
		}
		for i := end - scroll; i < h; i++ {
			b.WriteByte('\n')
		}
		b.WriteString(muted.Render(fit("↑/↓ scroll  q/Esc back", width)))
		return ui.NewAltScreenView(b.String())
	}

	b.WriteString(muted.Render(fit("  name                 ref                  state      last used  path", width)))
	b.WriteByte('\n')
	start, end := ui.VisibleRange(len(m.items), m.cursor, m.viewportHeight())
	for i := start; i < end; i++ {
		wt := m.items[i]
		mark := " "
		if samePath(wt.Dir, m.boundDir) {
			mark = "*"
		}
		ref := wt.Branch
		if ref == "" {
			ref = "detached@" + shortSHA(wt.HeadSHA)
		}
		state := string(wt.Status)
		if wt.DirtyFiles > 0 {
			state = fmt.Sprintf("dirty:%d", wt.DirtyFiles)
		} else if state == "" {
			state = "clean"
		}
		row := fit(fmt.Sprintf("%s %-20s %-20s %-10s %-10s %s", mark, wt.Name, ref, state, relative(wt.LastBoundAt), wt.Dir), width)
		if i == m.cursor {
			b.WriteString(selected.Render(row))
		} else {
			b.WriteString(row)
		}
		b.WriteByte('\n')
	}
	for i := end - start; i < m.viewportHeight(); i++ {
		b.WriteByte('\n')
	}
	b.WriteString(strings.Repeat("─", width))
	b.WriteByte('\n')
	footer := "[enter] open  [n] create  [i] details  [d] delete  [r] refresh  [q] back"
	confirming := m.mode == modeDelete || m.mode == modeForceDelete
	if m.mode == modeDelete {
		footer = fmt.Sprintf("Remove %s? This checks dirty state and active sessions. (y/n)", m.deleteTargetName())
	} else if m.mode == modeForceDelete {
		footer = "FORCE REMOVE? " + strings.Join(m.forceRisks, "; ") + " (y/n)"
	} else if (m.busy || m.refreshing) && m.status != "" {
		footer = m.status
	} else if m.err != nil {
		footer = "Error: " + m.err.Error()
	}
	if m.err != nil && !confirming {
		b.WriteString(errStyle.Render(fit(footer, width)))
	} else {
		b.WriteString(muted.Render(fit(footer, width)))
	}
	return ui.NewAltScreenView(b.String())
}

func (m *Model) deleteTargetName() string {
	if m.deleteTarget != nil {
		return m.deleteTarget.Name
	}
	return "worktree"
}
func (m *Model) detailLines() []string {
	if m.detail == nil {
		return []string{"No worktree selected."}
	}
	wt := m.detail
	ref := wt.Branch
	if ref == "" {
		ref = "detached"
	}
	lines := []string{
		"Name: " + wt.Name, "Path: " + wt.Dir, "Repository: " + wt.RepoRoot,
		"Ref: " + ref, "HEAD: " + wt.HeadSHA, "Base: " + wt.Base,
		fmt.Sprintf("Status: %s  Dirty files: %d  Orphaned: %t", wt.Status, wt.DirtyFiles, wt.Orphaned),
		"Created: " + formatTime(wt.CreatedAt), "Last bound: " + formatTime(wt.LastBoundAt), "", "Bound sessions:",
	}
	if m.detailBusy > 0 {
		lines = append(lines, "  Loading…")
	} else if m.usersErr != nil {
		lines = append(lines, "  Error: "+m.usersErr.Error())
	} else if len(m.detailUsers) == 0 {
		lines = append(lines, "  None")
	} else {
		for _, s := range m.detailUsers {
			name := s.Name
			if name == "" {
				name = s.ID
			}
			lines = append(lines, fmt.Sprintf("  #%d %s [%s]", s.Number, name, s.Status))
		}
	}
	lines = append(lines, "", "Diff:")
	if m.detailBusy > 0 {
		lines = append(lines, "  Loading…")
	} else if m.diffErr != nil {
		lines = append(lines, "  Error: "+m.diffErr.Error())
	} else if strings.TrimSpace(m.detailDiff) == "" {
		lines = append(lines, "  Clean (no changes)")
	} else {
		lines = append(lines, strings.Split(m.detailDiff, "\n")...)
	}
	return lines
}
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
func relative(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	current := ansi.StringWidth(s)
	if current > width {
		return ansi.Truncate(s, width, "...")
	}
	if current < width {
		return s + strings.Repeat(" ", width-current)
	}
	return s
}
