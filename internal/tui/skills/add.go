package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/ui"
	"golang.org/x/term"
)

// AddPhase represents the current phase of the add workflow
type AddPhase int

const (
	PhaseDiscovering AddPhase = iota
	PhaseSelectSkills
	PhaseSelectPaths
	PhaseInstalling
	PhaseDone
)

// Message types for the add TUI
type (
	discoverResultMsg struct {
		skills []skills.DiscoveredSkill
		err    error
	}
	installProgressMsg struct {
		skill   string
		current int
		total   int
	}
	installCompleteMsg struct {
		results []installResult
	}
)

type installResult struct {
	skillName string
	path      string
	err       error
}

// AddModel is the Bubble Tea model for the add workflow
type AddModel struct {
	width   int
	height  int
	spinner spinner.Model

	// State
	phase   AddPhase
	repoRef skills.GitHubRepoRef
	err     error
	message string

	// Discovery state
	discoveredSkills []skills.DiscoveredSkill
	skillCursor      int
	skillSelected    []bool

	// Path selection state
	installPaths  []InstallPath
	installCursor int

	// Install state
	installing      bool
	installProgress string
	installResults  []installResult

	// GitHub client
	client *skills.GitHubClient
}

// NewAddModel creates a new add workflow model
func NewAddModel(ref skills.GitHubRepoRef) *AddModel {
	// Get terminal size
	width := 80
	height := 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		width = w
		height = h
	}

	// Create spinner
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	return &AddModel{
		width:        width,
		height:       height,
		spinner:      s,
		phase:        PhaseDiscovering,
		repoRef:      ref,
		client:       skills.NewGitHubClient(),
		installPaths: BuildInstallPaths(),
	}
}

// Init implements tea.Model
func (m *AddModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		m.discoverSkills(),
	)
}

// Update implements tea.Model
func (m *AddModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case discoverResultMsg:
		if msg.err != nil {
			m.err = msg.err
			m.phase = PhaseDone
			return m, nil
		}
		m.discoveredSkills = msg.skills
		m.skillSelected = make([]bool, len(msg.skills))
		// Select all by default
		for i := range m.skillSelected {
			m.skillSelected[i] = true
		}
		m.phase = PhaseSelectSkills
		return m, nil

	case installCompleteMsg:
		m.installResults = msg.results
		m.phase = PhaseDone
		return m, nil
	}

	return m, nil
}

func (m *AddModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit

	case "esc":
		// Go back to previous phase or quit
		switch m.phase {
		case PhaseSelectPaths:
			m.phase = PhaseSelectSkills
		default:
			return m, tea.Quit
		}
		return m, nil
	}

	switch m.phase {
	case PhaseSelectSkills:
		return m.handleSkillSelection(msg)
	case PhaseSelectPaths:
		return m.handlePathSelection(msg)
	case PhaseDone:
		// Any key exits
		return m, tea.Quit
	}

	return m, nil
}

func (m *AddModel) handleSkillSelection(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.skillCursor > 0 {
			m.skillCursor--
		}
	case "down", "j":
		if m.skillCursor < len(m.discoveredSkills)-1 {
			m.skillCursor++
		}
	case " ", "space":
		// Toggle selection
		if m.skillCursor < len(m.skillSelected) {
			m.skillSelected[m.skillCursor] = !m.skillSelected[m.skillCursor]
		}
	case "a":
		// Select all
		for i := range m.skillSelected {
			m.skillSelected[i] = true
		}
	case "n":
		// Select none
		for i := range m.skillSelected {
			m.skillSelected[i] = false
		}
	case "enter":
		// Check if any skills are selected
		hasSelection := false
		for _, selected := range m.skillSelected {
			if selected {
				hasSelection = true
				break
			}
		}
		if hasSelection {
			m.phase = PhaseSelectPaths
			m.installCursor = 0
		}
	}
	return m, nil
}

func (m *AddModel) handlePathSelection(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.installCursor > 0 {
			m.installCursor--
		}
	case "down", "j":
		if m.installCursor < len(m.installPaths)-1 {
			m.installCursor++
		}
	case " ", "space":
		// Toggle selection
		if m.installCursor < len(m.installPaths) {
			m.installPaths[m.installCursor].Selected = !m.installPaths[m.installCursor].Selected
		}
	case "enter":
		// Check if any paths are selected
		hasSelection := false
		for _, p := range m.installPaths {
			if p.Selected {
				hasSelection = true
				break
			}
		}
		if hasSelection {
			m.phase = PhaseInstalling
			return m, m.installSkills()
		}
	}
	return m, nil
}

// View implements tea.Model
func (m *AddModel) View() tea.View {
	var b strings.Builder

	switch m.phase {
	case PhaseDiscovering:
		b.WriteString(m.viewDiscovering())
	case PhaseSelectSkills:
		b.WriteString(m.viewSelectSkills())
	case PhaseSelectPaths:
		b.WriteString(m.viewSelectPaths())
	case PhaseInstalling:
		b.WriteString(m.viewInstalling())
	case PhaseDone:
		b.WriteString(m.viewDone())
	}

	return ui.NewAltScreenView(b.String())
}

func (m *AddModel) viewDiscovering() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Skills Add"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("%s Discovering skills in %s/%s/%s...\n",
		m.spinner.View(), m.repoRef.Owner, m.repoRef.Repo, m.repoRef.Path))

	if !m.client.HasToken() {
		b.WriteString(mutedStyle.Render("\n  Tip: Set GITHUB_TOKEN for higher rate limits"))
	}

	return b.String()
}

func (m *AddModel) viewSelectSkills() string {
	selectedCount := 0
	for _, selected := range m.skillSelected {
		if selected {
			selectedCount++
		}
	}

	header := titleStyle.Render(fmt.Sprintf("Skills in %s/%s", m.repoRef.Owner, m.repoRef.Repo)) +
		"\n\n" +
		fmt.Sprintf("Found %d skill(s), %d selected:\n\n", len(m.discoveredSkills), selectedCount)
	footer := "\n" + helpStyle.Render("[Space] toggle  [a] all  [n] none  [Enter] install  [q] quit")
	availableRows := ui.RemainingHeight(m.height, header, footer)

	blocks := make([]string, 0, len(m.discoveredSkills))
	heights := make([]int, 0, len(m.discoveredSkills))
	for i, skill := range m.discoveredSkills {
		cursor := "  "
		if i == m.skillCursor {
			cursor = "> "
		}

		checkbox := "[ ]"
		if m.skillSelected[i] {
			checkbox = "[x]"
		}

		line := fmt.Sprintf("%s%s %s", cursor, checkbox, skill.Name)
		if i == m.skillCursor {
			line = selectedStyle.Render(line)
		}

		var item strings.Builder
		item.WriteString(line)
		item.WriteString(mutedStyle.Render(fmt.Sprintf(" (%d files)", skill.FileCount)))
		item.WriteString("\n")

		if skill.Description != "" {
			item.WriteString(mutedStyle.Render(fmt.Sprintf("      %s", skill.Description)))
			item.WriteString("\n")
		}

		block := item.String()
		blocks = append(blocks, block)
		heights = append(heights, lipgloss.Height(block))
	}

	start, end := ui.VisibleRangeByHeights(heights, m.skillCursor, availableRows)

	var b strings.Builder
	b.WriteString(header)
	for i := start; i < end; i++ {
		b.WriteString(blocks[i])
	}
	b.WriteString(footer)

	return b.String()
}

func (m *AddModel) viewSelectPaths() string {
	selectedCount := 0
	for _, selected := range m.skillSelected {
		if selected {
			selectedCount++
		}
	}

	header := titleStyle.Render("Select Install Location") +
		"\n\n" +
		fmt.Sprintf("Install %d skill(s) to:\n\n", selectedCount)
	footer := "\n" + helpStyle.Render("[Space] toggle  [Enter] confirm  [Esc] back  [q] quit")
	availableRows := ui.RemainingHeight(m.height, header, footer)
	start, end := ui.VisibleRange(len(m.installPaths), m.installCursor, availableRows)

	var b strings.Builder
	b.WriteString(header)
	for i := start; i < end; i++ {
		p := m.installPaths[i]
		cursor := "  "
		if i == m.installCursor {
			cursor = "> "
		}

		checkbox := "[ ]"
		if p.Selected {
			checkbox = "[x]"
		}

		displayPath := p.Path
		if home, _ := os.UserHomeDir(); home != "" {
			displayPath = strings.Replace(displayPath, home, "~", 1)
		}

		line := fmt.Sprintf("%s%s %s", cursor, checkbox, displayPath)
		if i == m.installCursor {
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString(line)
		}

		b.WriteString(mutedStyle.Render(fmt.Sprintf(" (%s)", p.Label)))
		b.WriteString("\n")
	}
	b.WriteString(footer)

	return b.String()
}

func (m *AddModel) viewInstalling() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Installing Skills"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("%s %s\n", m.spinner.View(), m.installProgress))
	return b.String()
}

func (m *AddModel) viewDone() string {
	var b strings.Builder

	if m.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("Error"))
		b.WriteString("\n\n")
		b.WriteString(m.err.Error())
		b.WriteString("\n")
		return b.String()
	}

	b.WriteString(installedStyle.Render("Installation Complete"))
	b.WriteString("\n\n")

	// Group results by status
	var succeeded, failed []installResult
	for _, r := range m.installResults {
		if r.err != nil {
			failed = append(failed, r)
		} else {
			succeeded = append(succeeded, r)
		}
	}

	if len(succeeded) > 0 {
		b.WriteString("Installed:\n")
		for _, r := range succeeded {
			displayPath := r.path
			if home, _ := os.UserHomeDir(); home != "" {
				displayPath = strings.Replace(displayPath, home, "~", 1)
			}
			b.WriteString(fmt.Sprintf("  %s -> %s\n", installedStyle.Render(r.skillName), displayPath))
		}
	}

	if len(failed) > 0 {
		b.WriteString("\nFailed:\n")
		for _, r := range failed {
			b.WriteString(fmt.Sprintf("  %s: %v\n",
				lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(r.skillName),
				r.err))
		}
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("Press any key to exit"))

	return b.String()
}

// Command functions

func (m *AddModel) discoverSkills() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		discovered, err := m.client.DiscoverSkills(ctx, m.repoRef)
		return discoverResultMsg{skills: discovered, err: err}
	}
}

func (m *AddModel) installSkills() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		var results []installResult

		// Get selected skills
		var selectedSkills []skills.DiscoveredSkill
		for i, skill := range m.discoveredSkills {
			if m.skillSelected[i] {
				selectedSkills = append(selectedSkills, skill)
			}
		}

		// Get selected paths
		var selectedPaths []InstallPath
		for _, p := range m.installPaths {
			if p.Selected {
				selectedPaths = append(selectedPaths, p)
			}
		}

		// Install each skill to each path
		for _, skill := range selectedSkills {
			for _, p := range selectedPaths {
				err := m.client.DownloadSkillDir(ctx, skill, p.Path)
				result := installResult{
					skillName: skill.Name,
					path:      filepath.Join(p.Path, skill.Name),
					err:       err,
				}

				if err == nil {
					// Inject provenance metadata
					skillDir := filepath.Join(p.Path, skill.Name)
					skillMDPath := filepath.Join(skillDir, "SKILL.md")
					if content, err := os.ReadFile(skillMDPath); err == nil {
						updatedContent := skills.InjectGitHubProvenance(content, skill)
						os.WriteFile(skillMDPath, updatedContent, 0644)
					}
				}

				results = append(results, result)
			}
		}

		return installCompleteMsg{results: results}
	}
}

// RunAdd starts the add TUI workflow
func RunAdd(ref skills.GitHubRepoRef) error {
	model := NewAddModel(ref)
	p := tea.NewProgram(model)
	_, err := p.Run()
	return err
}
