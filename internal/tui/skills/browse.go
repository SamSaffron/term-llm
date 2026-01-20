package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/skills"
	"golang.org/x/term"
)

const debounceDelay = 500 * time.Millisecond
const minSearchLength = 2 // Minimum characters before searching API

// Install destination paths
type InstallPath struct {
	ID       string // term-llm, local, claude, codex, gemini
	Label    string
	Path     string
	Selected bool
}

// Styles for the browser
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("10"))

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("14"))

	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245"))

	installedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245"))

	aiSearchStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("205"))

	categoryStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("33"))

	authorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245"))

	starsStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("226"))
)

// Message types
type (
	searchResultMsg struct {
		skills []skills.RemoteSkill
		query  string
		err    error
	}
	installResultMsg struct {
		remoteName string   // Original skill name from registry
		localName  string   // Name it was installed as (may be different)
		paths      []string // Where it was installed
		err        error
	}
	debounceTickMsg struct {
		query string
	}
	skillContentMsg struct {
		skill   skills.RemoteSkill
		content string
		err     error
	}
	deleteResultMsg struct {
		name  string
		paths []string
		err   error
	}
)

// ViewMode determines what the UI is showing
type ViewMode int

const (
	ViewBrowse ViewMode = iota
	ViewInstall
	ViewShow
	ViewDelete
)

// Model is the Bubble Tea model for the skills browser
type Model struct {
	width   int
	height  int
	input   textinput.Model
	spinner spinner.Model

	// State
	allSkills    []skills.RemoteSkill // All loaded skills from searches
	filteredList []skills.RemoteSkill // Filtered skills to display
	installed    map[string][]string  // Skills already installed locally -> paths
	cursor       int
	loading      bool
	searching    bool // True when a debounced search is pending
	err          error
	message      string

	// View mode
	viewMode ViewMode

	// Install picker state
	installPaths    []InstallPath
	installCursor   int
	installing      bool
	installName     textinput.Model
	installConflict string // Name of conflicting skill if any

	// Show view state
	showSkill      skills.RemoteSkill
	showContent    string
	showScrollY    int
	loadingContent bool

	// Delete view state
	deleteSkill skills.RemoteSkill
	deleting    bool

	// Registry client
	registry *skills.RemoteRegistryClient

	// Filter/Search
	filterText       string
	lastSearchText   string
	useAISearch      bool
	showingInstalled bool // True when showing installed skills (empty search)
}

// New creates a new skills browser model
func New(initialQuery string, useAISearch bool) *Model {
	// Get terminal size
	width := 80
	height := 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		width = w
		height = h
	}

	// Create text input for filtering
	ti := textinput.New()
	ti.Placeholder = "Type to search skills..."
	ti.Focus()
	ti.CharLimit = 100
	ti.Width = width - 20 // Leave room for AI toggle indicator
	ti.SetValue(initialQuery)

	// Create spinner
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	// Create install name input
	installNameInput := textinput.New()
	installNameInput.Placeholder = "skill-name"
	installNameInput.CharLimit = 50
	installNameInput.Width = 40

	// Build install paths
	installPaths := buildInstallPaths()

	// Check installed skills
	installed := checkInstalledSkills()

	return &Model{
		width:        width,
		height:       height,
		input:        ti,
		spinner:      s,
		registry:     skills.NewRemoteRegistryClient(),
		installed:    installed,
		filterText:   initialQuery,
		useAISearch:  useAISearch,
		installPaths: installPaths,
		installName:  installNameInput,
		viewMode:     ViewBrowse,
	}
}

// buildInstallPaths creates the list of available install destinations
func buildInstallPaths() []InstallPath {
	home, _ := os.UserHomeDir()
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" && home != "" {
		configDir = filepath.Join(home, ".config")
	}

	cwd, _ := os.Getwd()
	inProject := isProjectDirectory(cwd, home)

	var paths []InstallPath

	// term-llm global
	paths = append(paths, InstallPath{
		ID:       "term-llm",
		Label:    "term-llm global",
		Path:     filepath.Join(configDir, "term-llm", "skills"),
		Selected: true, // Default selected
	})

	// term-llm project-local (only if in a project)
	if inProject {
		paths = append(paths, InstallPath{
			ID:    "local",
			Label: "term-llm project",
			Path:  filepath.Join(cwd, ".skills"),
		})
	}

	if home != "" {
		// Claude Code
		paths = append(paths, InstallPath{
			ID:    "claude",
			Label: "Claude Code global",
			Path:  filepath.Join(home, ".claude", "skills"),
		})
		if inProject {
			paths = append(paths, InstallPath{
				ID:    "claude-local",
				Label: "Claude Code project",
				Path:  filepath.Join(cwd, ".claude", "skills"),
			})
		}

		// Codex CLI
		paths = append(paths, InstallPath{
			ID:    "codex",
			Label: "Codex CLI global",
			Path:  filepath.Join(home, ".codex", "skills"),
		})
		if inProject {
			paths = append(paths, InstallPath{
				ID:    "codex-local",
				Label: "Codex CLI project",
				Path:  filepath.Join(cwd, ".codex", "skills"),
			})
		}

		// Gemini CLI
		paths = append(paths, InstallPath{
			ID:    "gemini",
			Label: "Gemini CLI global",
			Path:  filepath.Join(home, ".gemini", "skills"),
		})
		if inProject {
			paths = append(paths, InstallPath{
				ID:    "gemini-local",
				Label: "Gemini CLI project",
				Path:  filepath.Join(cwd, ".gemini", "skills"),
			})
		}

		// Cursor
		paths = append(paths, InstallPath{
			ID:    "cursor",
			Label: "Cursor global",
			Path:  filepath.Join(home, ".cursor", "skills"),
		})
		if inProject {
			paths = append(paths, InstallPath{
				ID:    "cursor-local",
				Label: "Cursor project",
				Path:  filepath.Join(cwd, ".cursor", "skills"),
			})
		}
	}

	return paths
}

// isProjectDirectory checks if we should offer project-local install options
func isProjectDirectory(dir, home string) bool {
	// Don't offer project install if we're directly in HOME
	if dir == home {
		return false
	}

	// Don't offer if we're in XDG directories
	xdgDirs := []string{
		os.Getenv("XDG_CONFIG_HOME"),
		os.Getenv("XDG_CACHE_HOME"),
		os.Getenv("XDG_DATA_HOME"),
		os.Getenv("XDG_STATE_HOME"),
	}

	// Add fallback XDG paths
	if home != "" {
		xdgDirs = append(xdgDirs,
			filepath.Join(home, ".config"),
			filepath.Join(home, ".cache"),
			filepath.Join(home, ".local"),
		)
	}

	for _, xdg := range xdgDirs {
		if xdg != "" && (dir == xdg || strings.HasPrefix(dir, xdg+string(filepath.Separator))) {
			return false
		}
	}

	return true
}

// checkInstalledSkills checks which skills are already installed locally
// Returns a map of skill name -> list of installed paths
// Uses ListAll() to find ALL copies, not just the one that wins at runtime
func checkInstalledSkills() map[string][]string {
	installed := make(map[string][]string)

	// Use config that includes project skills for browsing
	cfg := skills.DefaultRegistryConfig()
	cfg.IncludeProjectSkills = true

	registry, err := skills.NewRegistry(cfg)
	if err != nil {
		return installed
	}

	skillList, err := registry.ListAll()
	if err != nil {
		return installed
	}

	home, _ := os.UserHomeDir()

	for _, skill := range skillList {
		// Shorten path for display
		displayPath := skill.SourcePath
		if home != "" {
			displayPath = strings.Replace(displayPath, home, "~", 1)
		}
		installed[skill.Name] = append(installed[skill.Name], displayPath)
	}

	return installed
}

// Init initializes the model
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		textinput.Blink,
		m.spinner.Tick,
	}

	// If there's an initial query, trigger a search
	if m.filterText != "" {
		m.loading = true
		cmds = append(cmds, m.loadSkills(m.filterText))
	} else {
		// No query - show installed skills
		m.applyFilter()
	}

	return tea.Batch(cmds...)
}

// Update handles messages
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = m.width - 20
		return m, nil

	case tea.KeyMsg:
		key := msg.String()

		// Global keys
		if key == "ctrl+c" {
			return m, tea.Quit
		}

		// Handle based on view mode
		switch m.viewMode {
		case ViewInstall:
			return m.updateInstallView(key, msg)
		case ViewShow:
			return m.updateShowView(key)
		case ViewDelete:
			return m.updateDeleteView(key)
		default:
			return m.updateBrowseView(key, msg)
		}

	case spinner.TickMsg:
		if m.loading || m.searching || m.installing || m.loadingContent || m.deleting {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	case debounceTickMsg:
		if msg.query == m.filterText && msg.query != m.lastSearchText {
			m.loading = true
			return m, m.loadSkills(msg.query)
		}
		m.searching = false

	case searchResultMsg:
		m.loading = false
		m.searching = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.mergeSkills(msg.skills)
			m.lastSearchText = msg.query
			m.err = nil
			m.applyFilter()
		}

	case installResultMsg:
		m.installing = false
		m.viewMode = ViewBrowse
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to install %s: %v", msg.localName, msg.err)
		} else {
			// Mark both remote and local names as installed with their paths
			m.installed[msg.remoteName] = append(m.installed[msg.remoteName], msg.paths...)
			if msg.localName != msg.remoteName {
				m.installed[msg.localName] = append(m.installed[msg.localName], msg.paths...)
			}

			// Build a satisfying success message
			if msg.remoteName != msg.localName {
				m.message = fmt.Sprintf("✓ Installed \"%s\" as \"%s\" to: %s", msg.remoteName, msg.localName, strings.Join(msg.paths, ", "))
			} else {
				m.message = fmt.Sprintf("✓ Installed \"%s\" to: %s", msg.localName, strings.Join(msg.paths, ", "))
			}
		}

	case skillContentMsg:
		m.loadingContent = false
		if msg.err != nil {
			m.showContent = fmt.Sprintf("(Could not load skill content: %v)", msg.err)
		} else {
			m.showContent = msg.content
		}

	case deleteResultMsg:
		m.deleting = false
		m.viewMode = ViewBrowse
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to delete %s: %v", msg.name, msg.err)
		} else {
			m.message = fmt.Sprintf("✓ Deleted \"%s\" from: %s", msg.name, strings.Join(msg.paths, ", "))
			// Refresh installed map and filtered list
			m.installed = checkInstalledSkills()
			m.applyFilter()
			// Adjust cursor if needed
			if m.cursor >= len(m.filteredList) && m.cursor > 0 {
				m.cursor = len(m.filteredList) - 1
			}
		}
	}

	return m, tea.Batch(cmds...)
}

// updateBrowseView handles keys in browse mode
func (m *Model) updateBrowseView(key string, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Tab toggles focus
	if key == "tab" {
		if m.input.Focused() {
			m.input.Blur()
		} else {
			m.input.Focus()
		}
		return m, nil
	}

	// Esc blurs input or quits
	if key == "esc" {
		if m.input.Focused() {
			m.input.Blur()
			return m, nil
		}
		return m, tea.Quit
	}

	// Ctrl+A toggles AI search (works in both focused and unfocused states)
	if key == "ctrl+a" {
		m.useAISearch = !m.useAISearch
		// Re-search if we have a query
		if len(m.filterText) >= minSearchLength {
			m.loading = true
			m.lastSearchText = "" // Force new search
			return m, m.loadSkills(m.filterText)
		}
		return m, nil
	}

	// When input is focused
	if m.input.Focused() {
		// Arrow keys blur input
		if key == "up" || key == "down" {
			m.input.Blur()
			return m, nil
		}

		// All other keys go to text input
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)

		// Apply filter and schedule debounced search
		if m.input.Value() != m.filterText {
			m.filterText = m.input.Value()
			m.applyFilter()

			// Only search API if query meets minimum length
			if len(m.filterText) >= minSearchLength && m.filterText != m.lastSearchText {
				m.searching = true
				cmds = append(cmds, m.debounceSearch(m.filterText))
			}
		}
		return m, tea.Batch(cmds...)
	}

	// Input NOT focused - handle navigation and actions
	switch key {
	case "q":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		} else {
			// At top of list - focus the filter input
			m.input.Focus()
		}
	case "down", "j":
		if m.cursor < len(m.filteredList)-1 {
			m.cursor++
		}
	case "i", "enter":
		if len(m.filteredList) > 0 && m.cursor < len(m.filteredList) {
			m.enterInstallView(m.filteredList[m.cursor])
		}
	case "s":
		// Show skill details in modal
		if len(m.filteredList) > 0 && m.cursor < len(m.filteredList) {
			skill := m.filteredList[m.cursor]
			m.showSkill = skill
			m.showContent = ""
			m.showScrollY = 0
			m.viewMode = ViewShow
			m.loadingContent = true
			return m, m.loadSkillContent(skill)
		}
	case "g":
		// Open GitHub
		if len(m.filteredList) > 0 && m.cursor < len(m.filteredList) {
			skill := m.filteredList[m.cursor]
			if skill.Repository != "" {
				openBrowser(skill.Repository)
				m.message = fmt.Sprintf("Opened %s in browser", skill.Repository)
			} else if skill.URL != "" {
				openBrowser(skill.URL)
				m.message = fmt.Sprintf("Opened %s in browser", skill.URL)
			}
		}
	case "d":
		// Delete installed skill
		if len(m.filteredList) > 0 && m.cursor < len(m.filteredList) {
			skill := m.filteredList[m.cursor]
			if len(m.installed[skill.Name]) > 0 {
				m.deleteSkill = skill
				m.viewMode = ViewDelete
			} else {
				m.message = fmt.Sprintf("\"%s\" is not installed locally", skill.Name)
			}
		}
	}

	return m, nil
}

// updateInstallView handles keys in install picker mode
func (m *Model) updateInstallView(key string, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle name input when focused
	if m.installName.Focused() {
		switch key {
		case "esc":
			m.viewMode = ViewBrowse
			return m, nil
		case "tab", "down":
			m.installName.Blur()
			return m, nil
		case "enter":
			// Move to path selection
			m.installName.Blur()
			return m, nil
		default:
			// Pass full message to text input
			var cmd tea.Cmd
			m.installName, cmd = m.installName.Update(msg)
			// Check for conflicts on name change
			m.checkNameConflict(m.installName.Value())
			return m, cmd
		}
	}

	// Path selection mode
	switch key {
	case "esc":
		m.viewMode = ViewBrowse
		return m, nil
	case "tab", "up":
		if key == "up" && m.installCursor > 0 {
			m.installCursor--
		} else if key == "tab" || (key == "up" && m.installCursor == 0) {
			m.installName.Focus()
		}
	case "down", "j":
		if m.installCursor < len(m.installPaths)-1 {
			m.installCursor++
		}
	case "k":
		if m.installCursor > 0 {
			m.installCursor--
		} else {
			m.installName.Focus()
		}
	case " ": // Space to toggle
		m.installPaths[m.installCursor].Selected = !m.installPaths[m.installCursor].Selected
	case "enter":
		// Install to selected paths
		var selectedPaths []InstallPath
		for _, p := range m.installPaths {
			if p.Selected {
				selectedPaths = append(selectedPaths, p)
			}
		}
		installName := m.installName.Value()
		if installName == "" {
			m.message = "Enter a skill name"
			return m, nil
		}
		// Validate name per Agent Skills spec
		if err := skills.ValidateName(installName); err != nil {
			m.message = fmt.Sprintf("Invalid name: %s", err)
			return m, nil
		}
		if len(selectedPaths) > 0 && m.cursor < len(m.filteredList) {
			m.installing = true
			return m, m.installSkillWithName(m.filteredList[m.cursor], selectedPaths, installName)
		}
		m.message = "Select at least one destination"
	}

	return m, nil
}

// updateShowView handles keys in show view
func (m *Model) updateShowView(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "q":
		m.viewMode = ViewBrowse
		return m, nil
	case "up", "k":
		if m.showScrollY > 0 {
			m.showScrollY--
		}
	case "down", "j":
		m.showScrollY++
	case "i", "enter":
		// Install from show view
		m.enterInstallView(m.showSkill)
	case "g":
		// Open GitHub
		if m.showSkill.Repository != "" {
			openBrowser(m.showSkill.Repository)
		} else if m.showSkill.URL != "" {
			openBrowser(m.showSkill.URL)
		}
	}
	return m, nil
}

// updateDeleteView handles keys in delete confirmation view
func (m *Model) updateDeleteView(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "n":
		m.viewMode = ViewBrowse
		return m, nil
	case "y":
		// Perform delete
		m.deleting = true
		return m, m.performDelete(m.deleteSkill)
	}
	return m, nil
}

// View renders the model
func (m *Model) View() string {
	switch m.viewMode {
	case ViewInstall:
		return m.viewInstall()
	case ViewShow:
		return m.viewShow()
	case ViewDelete:
		return m.viewDelete()
	default:
		return m.viewBrowse()
	}
}

// viewBrowse renders the browse view
func (m *Model) viewBrowse() string {
	var b strings.Builder

	// Title - changes based on mode
	if m.showingInstalled {
		b.WriteString(titleStyle.Render("Installed Skills"))
		b.WriteString(mutedStyle.Render(" (type to search marketplace)"))
	} else {
		b.WriteString(titleStyle.Render("Skills Browser"))
		b.WriteString(mutedStyle.Render(" (SkillsMP.com)"))
	}
	b.WriteString("\n\n")

	// Search input with AI toggle indicator
	b.WriteString(m.input.View())
	b.WriteString(" ")
	if m.useAISearch {
		b.WriteString(aiSearchStyle.Render("[AI]"))
	} else {
		b.WriteString(mutedStyle.Render("[AI: off]"))
	}
	b.WriteString("\n")

	// Status line
	if m.loading {
		b.WriteString(m.spinner.View())
		if m.useAISearch {
			b.WriteString(" AI searching...")
		} else {
			b.WriteString(" Searching...")
		}
		b.WriteString("\n")
	} else if m.searching {
		b.WriteString(m.spinner.View())
		b.WriteString(" ...")
		b.WriteString("\n")
	} else {
		b.WriteString("\n")
	}

	// Error
	if m.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
	}

	// Message
	if m.message != "" {
		b.WriteString(m.message)
		b.WriteString("\n")
	}

	// Skills list
	if len(m.filteredList) > 0 {
		availableHeight := m.height - 10
		if m.err != nil {
			availableHeight--
		}
		if m.message != "" {
			availableHeight--
		}

		visibleSkills := availableHeight / 3 // Each skill takes ~3 lines
		if visibleSkills < 2 {
			visibleSkills = 2
		}

		startIdx := 0
		if m.cursor >= visibleSkills {
			startIdx = m.cursor - visibleSkills + 1
		}
		endIdx := startIdx + visibleSkills
		if endIdx > len(m.filteredList) {
			endIdx = len(m.filteredList)
		}

		for i := startIdx; i < endIdx; i++ {
			skill := m.filteredList[i]

			// Cursor
			cursor := "  "
			if i == m.cursor && !m.input.Focused() {
				cursor = selectedStyle.Render("> ")
			}
			b.WriteString(cursor)

			// Skill name
			if i == m.cursor && !m.input.Focused() {
				b.WriteString(selectedStyle.Render(skill.Name))
			} else {
				b.WriteString(skill.Name)
			}

			// Installed badge with count
			if paths := m.installed[skill.Name]; len(paths) > 0 {
				b.WriteString(" ")
				if len(paths) == 1 {
					b.WriteString(installedStyle.Render("[installed]"))
				} else {
					b.WriteString(installedStyle.Render(fmt.Sprintf("[installed %d]", len(paths))))
				}
			}

			// Stars
			if skill.Stars > 0 {
				b.WriteString(" ")
				b.WriteString(starsStyle.Render(fmt.Sprintf("★%d", skill.Stars)))
			}
			b.WriteString("\n")

			// Author and category
			b.WriteString("    ")
			if skill.Author != "" {
				b.WriteString(authorStyle.Render("by " + skill.Author))
			}
			if skill.Category != "" {
				if skill.Author != "" {
					b.WriteString(" | ")
				}
				b.WriteString(categoryStyle.Render(skill.Category))
			}
			b.WriteString("\n")

			// Description
			desc := skill.Description
			if desc == "" {
				desc = "(no description)"
			}
			maxLen := m.width - 6
			if len(desc) > maxLen {
				desc = desc[:maxLen-3] + "..."
			}
			b.WriteString("    ")
			b.WriteString(mutedStyle.Render(desc))
			b.WriteString("\n")
		}

		// Show count
		if len(m.filteredList) > visibleSkills {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("\n  Showing %d-%d of %d skills", startIdx+1, endIdx, len(m.filteredList))))
			b.WriteString("\n")
		}

	} else if !m.loading && m.err == nil {
		if m.filterText != "" {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("No skills match '%s'. Try a different search.", m.filterText)))
			b.WriteString("\n")
		} else if m.showingInstalled {
			b.WriteString(mutedStyle.Render("No skills installed yet. Type to search the marketplace."))
			b.WriteString("\n")
		}
	}

	// Help
	b.WriteString("\n")
	if m.input.Focused() {
		b.WriteString(helpStyle.Render("Type to search • [↑↓] select • [Ctrl+A] toggle AI • [Esc] quit"))
	} else {
		b.WriteString(helpStyle.Render("[Enter/i] install  [s] show  [d] delete  [g] GitHub  [Ctrl+A] AI  [Tab] filter  [q] quit"))
	}
	b.WriteString("\n")

	return b.String()
}

// viewInstall renders the install destination picker
func (m *Model) viewInstall() string {
	var b strings.Builder

	skill := m.filteredList[m.cursor]

	b.WriteString(titleStyle.Render(fmt.Sprintf("Install \"%s\"", skill.Name)))
	b.WriteString("\n\n")

	if m.installing {
		b.WriteString(m.spinner.View())
		b.WriteString(" Installing...")
		b.WriteString("\n\n")
	}

	// Name input
	b.WriteString(mutedStyle.Render("Name: "))
	b.WriteString(m.installName.View())
	b.WriteString("\n")

	// Conflict warning
	if m.installConflict != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Render(
			fmt.Sprintf("⚠ Warning: \"%s\" already exists locally (will overwrite)", m.installConflict)))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Destinations header
	b.WriteString(mutedStyle.Render("Install to:"))
	b.WriteString("\n")

	for i, path := range m.installPaths {
		// Cursor (only show when name input not focused)
		cursor := "  "
		if i == m.installCursor && !m.installName.Focused() {
			cursor = selectedStyle.Render("> ")
		}
		b.WriteString(cursor)

		// Checkbox
		if path.Selected {
			b.WriteString(installedStyle.Render("[x] "))
		} else {
			b.WriteString("[ ] ")
		}

		// Path display
		displayPath := path.Path
		// Shorten home directory
		if home, _ := os.UserHomeDir(); home != "" {
			displayPath = strings.Replace(displayPath, home, "~", 1)
		}

		if i == m.installCursor && !m.installName.Focused() {
			b.WriteString(selectedStyle.Render(displayPath))
		} else {
			b.WriteString(displayPath)
		}

		// Label
		b.WriteString("  ")
		b.WriteString(mutedStyle.Render("(" + path.Label + ")"))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if m.installName.Focused() {
		b.WriteString(helpStyle.Render("[Tab/↓] destinations  [Enter] confirm  [Esc] cancel"))
	} else {
		b.WriteString(helpStyle.Render("[Tab/↑] edit name  [Space] toggle  [Enter] confirm  [Esc] cancel"))
	}
	b.WriteString("\n")

	return b.String()
}

// viewShow renders the skill detail modal with full-view scrolling
func (m *Model) viewShow() string {
	skill := m.showSkill

	// Build all content first
	var content strings.Builder

	// Title bar
	content.WriteString(titleStyle.Render(fmt.Sprintf("─── %s ───", skill.Name)))
	content.WriteString("\n\n")

	// Metadata section
	if skill.Author != "" {
		content.WriteString(mutedStyle.Render("Author: "))
		content.WriteString(skill.Author)
		content.WriteString("\n")
	}

	if skill.Category != "" {
		content.WriteString(mutedStyle.Render("Category: "))
		content.WriteString(categoryStyle.Render(skill.Category))
		content.WriteString("\n")
	}

	if skill.Stars > 0 {
		content.WriteString(mutedStyle.Render("Stars: "))
		content.WriteString(starsStyle.Render(fmt.Sprintf("★ %d", skill.Stars)))
		content.WriteString("\n")
	}

	if skill.Repository != "" {
		content.WriteString(mutedStyle.Render("GitHub: "))
		content.WriteString(skill.Repository)
		content.WriteString("\n")
	}

	if skill.URL != "" {
		content.WriteString(mutedStyle.Render("URL: "))
		content.WriteString(skill.URL)
		content.WriteString("\n")
	}

	if paths := m.installed[skill.Name]; len(paths) > 0 {
		content.WriteString(installedStyle.Render("✓ Installed locally:"))
		content.WriteString("\n")
		for _, p := range paths {
			content.WriteString("    ")
			content.WriteString(p)
			content.WriteString("\n")
		}
	}

	content.WriteString("\n")

	// Description
	if skill.Description != "" {
		content.WriteString(mutedStyle.Render("Description:"))
		content.WriteString("\n")
		content.WriteString(skill.Description)
		content.WriteString("\n\n")
	}

	// Content section
	content.WriteString(mutedStyle.Render("─── SKILL.md Content ───"))
	content.WriteString("\n\n")

	if m.loadingContent {
		content.WriteString(m.spinner.View())
		content.WriteString(" Loading skill content...")
		content.WriteString("\n")
	} else if m.showContent != "" {
		content.WriteString(m.showContent)
	} else {
		content.WriteString(mutedStyle.Render("(No content available)"))
		content.WriteString("\n")
	}

	// Now apply scrolling to the entire content
	lines := strings.Split(content.String(), "\n")

	// Calculate visible area (leave room for help bar)
	visibleLines := m.height - 4
	if visibleLines < 5 {
		visibleLines = 5
	}

	// Clamp scroll position
	maxScroll := len(lines) - visibleLines
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.showScrollY > maxScroll {
		m.showScrollY = maxScroll
	}

	// Build output with scrolled view
	var b strings.Builder

	endIdx := m.showScrollY + visibleLines
	if endIdx > len(lines) {
		endIdx = len(lines)
	}

	for i := m.showScrollY; i < endIdx; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
	}

	// Show scroll indicator if needed
	if len(lines) > visibleLines {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("─── %d-%d of %d lines (↑↓ scroll) ───", m.showScrollY+1, endIdx, len(lines))))
		b.WriteString("\n")
	}

	// Help (always visible at bottom)
	b.WriteString(helpStyle.Render("[Enter/i] install  [g] GitHub  [↑↓] scroll  [Esc/q] back"))
	b.WriteString("\n")

	return b.String()
}

// viewDelete renders the delete confirmation view
func (m *Model) viewDelete() string {
	var b strings.Builder
	skill := m.deleteSkill
	paths := m.installed[skill.Name]

	b.WriteString(titleStyle.Render(fmt.Sprintf("Delete \"%s\"?", skill.Name)))
	b.WriteString("\n\n")

	if m.deleting {
		b.WriteString(m.spinner.View())
		b.WriteString(" Deleting...")
		b.WriteString("\n\n")
	}

	b.WriteString(mutedStyle.Render("This will remove the skill from:"))
	b.WriteString("\n\n")

	for _, p := range paths {
		b.WriteString("    ")
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("✗ "))
		b.WriteString(p)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("[y] confirm delete  [n/Esc] cancel"))
	b.WriteString("\n")

	return b.String()
}

// debounceSearch returns a command that fires after a delay
func (m *Model) debounceSearch(query string) tea.Cmd {
	return tea.Tick(debounceDelay, func(t time.Time) tea.Msg {
		return debounceTickMsg{query: query}
	})
}

// loadSkills loads skills from the registry
func (m *Model) loadSkills(query string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var result *skills.RemoteSearchResult
		var err error

		if m.useAISearch {
			result, err = m.registry.AISearch(ctx, query)
		} else {
			result, err = m.registry.Search(ctx, query)
		}

		if err != nil {
			return searchResultMsg{err: err, query: query}
		}
		return searchResultMsg{skills: result.Skills, query: query}
	}
}

// loadSkillContent fetches the SKILL.md content for a skill
func (m *Model) loadSkillContent(skill skills.RemoteSkill) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content, err := m.registry.DownloadSkill(ctx, &skill)
		if err != nil {
			return skillContentMsg{skill: skill, err: err}
		}
		return skillContentMsg{skill: skill, content: string(content)}
	}
}

// performDelete deletes a skill from all installed locations
func (m *Model) performDelete(skill skills.RemoteSkill) tea.Cmd {
	paths := m.installed[skill.Name]
	return func() tea.Msg {
		var deletedPaths []string
		home, _ := os.UserHomeDir()

		for _, displayPath := range paths {
			// Convert display path back to real path
			realPath := displayPath
			if home != "" && strings.HasPrefix(displayPath, "~") {
				realPath = filepath.Join(home, displayPath[1:])
			}

			// Remove the skill directory
			if err := os.RemoveAll(realPath); err != nil {
				return deleteResultMsg{name: skill.Name, err: fmt.Errorf("remove %s: %w", displayPath, err)}
			}
			deletedPaths = append(deletedPaths, displayPath)
		}

		return deleteResultMsg{name: skill.Name, paths: deletedPaths}
	}
}

// enterInstallView sets up and enters the install view for a skill
func (m *Model) enterInstallView(skill skills.RemoteSkill) {
	m.viewMode = ViewInstall
	m.installCursor = 0

	// Reset path selections to default
	for i := range m.installPaths {
		m.installPaths[i].Selected = (i == 0)
	}

	// Set up name input with skill's original name
	m.installName.SetValue(skill.Name)
	m.installName.Focus()

	// Check for conflicts
	m.checkNameConflict(skill.Name)
}

// checkNameConflict checks if a skill name conflicts with an existing skill
func (m *Model) checkNameConflict(name string) {
	if len(m.installed[name]) > 0 {
		m.installConflict = name
	} else {
		m.installConflict = ""
	}
}

// mergeSkills adds new skills to allSkills, deduplicating by name
func (m *Model) mergeSkills(newSkills []skills.RemoteSkill) {
	seen := make(map[string]bool)

	for _, s := range m.allSkills {
		seen[s.Name] = true
	}

	for _, s := range newSkills {
		if !seen[s.Name] {
			seen[s.Name] = true
			m.allSkills = append(m.allSkills, s)
		}
	}
}

// applyFilter filters allSkills based on filterText
func (m *Model) applyFilter() {
	if m.filterText == "" {
		// When filter is empty, show installed skills
		m.filteredList = m.getInstalledAsRemote()
		m.showingInstalled = true
	} else {
		m.showingInstalled = false
		filter := strings.ToLower(m.filterText)
		var filtered []skills.RemoteSkill
		for _, s := range m.allSkills {
			name := strings.ToLower(s.Name)
			desc := strings.ToLower(s.Description)
			author := strings.ToLower(s.Author)
			category := strings.ToLower(s.Category)
			if strings.Contains(name, filter) ||
				strings.Contains(desc, filter) ||
				strings.Contains(author, filter) ||
				strings.Contains(category, filter) {
				filtered = append(filtered, s)
			}
		}
		m.filteredList = filtered
	}

	// Sort: for installed view, sort alphabetically; for search, sort by stars
	if m.showingInstalled {
		sort.Slice(m.filteredList, func(i, j int) bool {
			return strings.ToLower(m.filteredList[i].Name) < strings.ToLower(m.filteredList[j].Name)
		})
	} else {
		sort.Slice(m.filteredList, func(i, j int) bool {
			if m.filteredList[i].Stars != m.filteredList[j].Stars {
				return m.filteredList[i].Stars > m.filteredList[j].Stars
			}
			return strings.ToLower(m.filteredList[i].Name) < strings.ToLower(m.filteredList[j].Name)
		})
	}

	if m.cursor >= len(m.filteredList) {
		m.cursor = 0
	}
}

// getInstalledAsRemote converts installed skills to RemoteSkill format for display
// Shows one entry per unique skill name (not per path) for cleaner browsing
func (m *Model) getInstalledAsRemote() []skills.RemoteSkill {
	var result []skills.RemoteSkill
	seen := make(map[string]bool)

	// Use config that includes project skills for browsing
	cfg := skills.DefaultRegistryConfig()
	cfg.IncludeProjectSkills = true

	registry, err := skills.NewRegistry(cfg)
	if err != nil {
		return result
	}

	// Use ListAll to find all copies, but dedupe by name for display
	skillList, err := registry.ListAll()
	if err != nil {
		return result
	}

	for _, skill := range skillList {
		if seen[skill.Name] {
			continue
		}
		seen[skill.Name] = true

		// Convert to RemoteSkill for display
		rs := skills.RemoteSkill{
			Name:        skill.Name,
			Description: skill.Description,
		}

		// Extract provenance info if available
		if skill.Metadata != nil {
			if author := skill.Metadata["_provenance_author"]; author != "" {
				rs.Author = author
			}
			if rawURL := skill.Metadata["_provenance_raw_url"]; rawURL != "" {
				rs.RawURL = rawURL
			}
			if category := skill.Metadata["_provenance_category"]; category != "" {
				rs.Category = category
			}
			if repo := skill.Metadata["_provenance_repository"]; repo != "" {
				rs.Repository = repo
			}
		}

		result = append(result, rs)
	}

	return result
}

// installSkillWithName downloads and installs a skill with a custom name to selected paths
func (m *Model) installSkillWithName(skill skills.RemoteSkill, paths []InstallPath, installName string) tea.Cmd {
	remoteName := skill.Name // Capture for the closure
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Download skill content
		content, err := m.registry.DownloadSkill(ctx, &skill)
		if err != nil {
			return installResultMsg{remoteName: remoteName, localName: installName, err: err}
		}

		// Inject provenance metadata into the SKILL.md frontmatter
		content = injectProvenance(content, skill, installName)

		var installedPaths []string

		// Install to each selected path
		for _, p := range paths {
			skillDir := filepath.Join(p.Path, installName)

			// Create directory
			if err := os.MkdirAll(skillDir, 0755); err != nil {
				return installResultMsg{remoteName: remoteName, localName: installName, err: fmt.Errorf("create %s: %w", skillDir, err)}
			}

			// Write SKILL.md
			skillPath := filepath.Join(skillDir, "SKILL.md")
			if err := os.WriteFile(skillPath, content, 0644); err != nil {
				return installResultMsg{remoteName: remoteName, localName: installName, err: fmt.Errorf("write %s: %w", skillPath, err)}
			}

			// Shorten for display
			displayPath := p.Path
			if home, _ := os.UserHomeDir(); home != "" {
				displayPath = strings.Replace(displayPath, home, "~", 1)
			}
			installedPaths = append(installedPaths, displayPath)
		}

		return installResultMsg{remoteName: remoteName, localName: installName, paths: installedPaths}
	}
}

// injectProvenance adds provenance metadata to SKILL.md frontmatter
func injectProvenance(content []byte, skill skills.RemoteSkill, installName string) []byte {
	contentStr := string(content)

	// Find the closing --- of frontmatter
	// Format: ---\n<frontmatter>\n---\n<body>
	parts := strings.SplitN(contentStr, "---", 3)
	if len(parts) < 3 {
		// No valid frontmatter, return as-is
		return content
	}

	frontmatter := parts[1]
	body := parts[2]

	// Update name if different
	if installName != skill.Name {
		frontmatter = strings.Replace(frontmatter, fmt.Sprintf("name: %s", skill.Name), fmt.Sprintf("name: %s", installName), 1)
		frontmatter = strings.Replace(frontmatter, fmt.Sprintf("name: \"%s\"", skill.Name), fmt.Sprintf("name: \"%s\"", installName), 1)
	}

	// Build provenance metadata block
	// Per spec, metadata is a map of string keys to string values
	var provenance strings.Builder
	provenance.WriteString("metadata:\n")
	provenance.WriteString(fmt.Sprintf("  _provenance_source: skillsmp\n"))
	provenance.WriteString(fmt.Sprintf("  _provenance_remote_name: %s\n", skill.Name))
	if skill.ID != "" {
		provenance.WriteString(fmt.Sprintf("  _provenance_skill_id: %s\n", skill.ID))
	}
	if skill.Repository != "" {
		provenance.WriteString(fmt.Sprintf("  _provenance_repository: %s\n", skill.Repository))
	}
	if skill.RawURL != "" {
		provenance.WriteString(fmt.Sprintf("  _provenance_raw_url: %s\n", skill.RawURL))
	}
	if skill.URL != "" {
		provenance.WriteString(fmt.Sprintf("  _provenance_url: %s\n", skill.URL))
	}
	if skill.Author != "" {
		provenance.WriteString(fmt.Sprintf("  _provenance_author: %s\n", skill.Author))
	}
	if skill.Category != "" {
		provenance.WriteString(fmt.Sprintf("  _provenance_category: %s\n", skill.Category))
	}
	provenance.WriteString(fmt.Sprintf("  _provenance_installed_at: %s\n", time.Now().UTC().Format(time.RFC3339)))
	if skill.UpdatedAt > 0 {
		provenance.WriteString(fmt.Sprintf("  _provenance_remote_updated_at: \"%.0f\"\n", skill.UpdatedAt))
	}

	// Check if metadata already exists in frontmatter
	if strings.Contains(frontmatter, "metadata:") {
		// Append our provenance fields to existing metadata
		// Find the metadata: line and inject after it
		lines := strings.Split(frontmatter, "\n")
		var newLines []string
		inMetadata := false
		provenanceAdded := false

		for _, line := range lines {
			newLines = append(newLines, line)
			trimmed := strings.TrimSpace(line)

			if trimmed == "metadata:" {
				inMetadata = true
				continue
			}

			if inMetadata && !provenanceAdded {
				// Add provenance fields right after metadata:
				newLines = append(newLines,
					fmt.Sprintf("  _provenance_source: skillsmp"),
					fmt.Sprintf("  _provenance_remote_name: %s", skill.Name),
				)
				if skill.ID != "" {
					newLines = append(newLines, fmt.Sprintf("  _provenance_skill_id: %s", skill.ID))
				}
				if skill.Repository != "" {
					newLines = append(newLines, fmt.Sprintf("  _provenance_repository: %s", skill.Repository))
				}
				if skill.RawURL != "" {
					newLines = append(newLines, fmt.Sprintf("  _provenance_raw_url: %s", skill.RawURL))
				}
				if skill.URL != "" {
					newLines = append(newLines, fmt.Sprintf("  _provenance_url: %s", skill.URL))
				}
				if skill.Author != "" {
					newLines = append(newLines, fmt.Sprintf("  _provenance_author: %s", skill.Author))
				}
				if skill.Category != "" {
					newLines = append(newLines, fmt.Sprintf("  _provenance_category: %s", skill.Category))
				}
				newLines = append(newLines, fmt.Sprintf("  _provenance_installed_at: %s", time.Now().UTC().Format(time.RFC3339)))
				if skill.UpdatedAt > 0 {
					newLines = append(newLines, fmt.Sprintf("  _provenance_remote_updated_at: \"%.0f\"", skill.UpdatedAt))
				}
				provenanceAdded = true
			}
		}
		frontmatter = strings.Join(newLines, "\n")
	} else {
		// No existing metadata, append our block
		frontmatter = strings.TrimRight(frontmatter, "\n") + "\n" + provenance.String()
	}

	return []byte("---" + frontmatter + "---" + body)
}

// InjectProvenanceFromMetadata injects provenance metadata from existing metadata map
// into new SKILL.md content. Used for updates to preserve provenance while updating content.
func InjectProvenanceFromMetadata(content []byte, metadata map[string]string, localName string) []byte {
	contentStr := string(content)

	// Find the closing --- of frontmatter
	parts := strings.SplitN(contentStr, "---", 3)
	if len(parts) < 3 {
		return content
	}

	frontmatter := parts[1]
	body := parts[2]

	// Update name to local name
	remoteName := metadata["_provenance_remote_name"]
	if remoteName != "" && localName != remoteName {
		frontmatter = strings.Replace(frontmatter, fmt.Sprintf("name: %s", remoteName), fmt.Sprintf("name: %s", localName), 1)
		frontmatter = strings.Replace(frontmatter, fmt.Sprintf("name: \"%s\"", remoteName), fmt.Sprintf("name: \"%s\"", localName), 1)
	}

	// Build provenance metadata block from existing metadata
	var provenance strings.Builder
	provenance.WriteString("metadata:\n")

	// Copy all _provenance_ fields
	provenanceKeys := []string{
		"_provenance_source",
		"_provenance_remote_name",
		"_provenance_skill_id",
		"_provenance_repository",
		"_provenance_raw_url",
		"_provenance_url",
		"_provenance_author",
		"_provenance_category",
	}
	for _, key := range provenanceKeys {
		if val := metadata[key]; val != "" {
			provenance.WriteString(fmt.Sprintf("  %s: %s\n", key, val))
		}
	}

	// Update installed_at timestamp
	provenance.WriteString(fmt.Sprintf("  _provenance_installed_at: %s\n", time.Now().UTC().Format(time.RFC3339)))

	// Keep remote_updated_at if present
	if val := metadata["_provenance_remote_updated_at"]; val != "" {
		provenance.WriteString(fmt.Sprintf("  _provenance_remote_updated_at: \"%s\"\n", val))
	}

	// Check if metadata already exists in frontmatter
	if strings.Contains(frontmatter, "metadata:") {
		// Remove existing metadata block and add new one
		lines := strings.Split(frontmatter, "\n")
		var newLines []string
		inMetadata := false

		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "metadata:" {
				inMetadata = true
				continue
			}
			if inMetadata {
				// Skip lines that are indented (part of metadata)
				if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
					continue
				}
				inMetadata = false
			}
			newLines = append(newLines, line)
		}
		frontmatter = strings.Join(newLines, "\n")
	}

	// Append our provenance block
	frontmatter = strings.TrimRight(frontmatter, "\n") + "\n" + provenance.String()

	return []byte("---" + frontmatter + "---" + body)
}

// openBrowser opens a URL in the default browser
func openBrowser(url string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}

	cmd.Start()
}

// RunBrowser runs the skills browser TUI
func RunBrowser(initialQuery string, useAISearch bool) error {
	model := New(initialQuery, useAISearch)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
