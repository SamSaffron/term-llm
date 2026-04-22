package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/ui"
	"golang.org/x/term"
)

const debounceDelay = 300 * time.Millisecond

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
)

// Message types
type (
	searchResultMsg struct {
		servers []mcp.RegistryServerWrapper
		query   string // The query that triggered this result
		err     error
	}
	installResultMsg struct {
		name string
		err  error
	}
	uninstallResultMsg struct {
		name string
		err  error
	}
	testResultMsg struct {
		name  string
		tools []string
		err   error
	}
	debounceTickMsg struct {
		query string
	}
)

// Model is the Bubble Tea model for the MCP browser
type Model struct {
	width   int
	height  int
	input   textinput.Model
	spinner spinner.Model

	// State
	allServers []mcp.RegistryServerWrapper // All loaded servers (merged from searches)
	servers    []mcp.RegistryServerWrapper // Filtered servers to display
	installed  map[string]bool
	cursor     int
	loading    bool
	testing    bool
	searching  bool // True when a debounced search is pending
	err        error
	message    string

	// Registry client
	registry *mcp.RegistryClient
	config   *mcp.Config

	// Filter/Search
	filterText     string
	lastSearchText string // Last query sent to the API

	// Bundled servers (preloaded for offline/fallback)
	bundledLoaded bool
}

// New creates a new MCP browser model
func New(initialQuery string) *Model {
	// Get terminal size
	width := 80
	height := 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		width = w
		height = h
	}

	// Create text input for filtering
	ti := textinput.New()
	ti.Placeholder = "Type to filter/search..."
	ti.Focus()
	ti.CharLimit = 100
	ti.SetWidth(max(1, width-4))
	ti.SetValue(initialQuery)

	// Create spinner
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	// Load config and installed servers
	cfg, _ := mcp.LoadConfig()
	installed := make(map[string]bool)
	if cfg != nil {
		for name := range cfg.Servers {
			installed[name] = true
		}
	}

	return &Model{
		width:      width,
		height:     height,
		input:      ti,
		spinner:    s,
		registry:   mcp.NewRegistryClient(),
		config:     cfg,
		installed:  installed,
		filterText: initialQuery,
	}
}

// Init initializes the model
func (m *Model) Init() tea.Cmd {
	// Load bundled servers immediately (synchronous, in-memory)
	// No network call on init - only search registry when user types
	m.loadBundledServers()

	cmds := []tea.Cmd{
		textinput.Blink,
		m.spinner.Tick,
	}

	// If there's an initial query, trigger a registry search
	if m.filterText != "" {
		m.loading = true
		cmds = append(cmds, m.loadServers(m.filterText))
	}

	return tea.Batch(cmds...)
}

// loadBundledServers adds the curated bundled servers to allServers
func (m *Model) loadBundledServers() {
	if m.bundledLoaded {
		return
	}
	bundled := mcp.GetBundledAsRegistryWrappers()
	m.mergeServers(bundled)
	m.bundledLoaded = true
	m.applyFilter()
}

// Update handles messages
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(max(1, m.width-4))
		return m, nil

	case tea.KeyPressMsg:
		key := msg.String()

		// Global keys that always work
		if key == "ctrl+c" {
			return m, tea.Quit
		}

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

		// When input is focused, send keys to it (except arrow keys which blur)
		if m.input.Focused() {
			// Arrow keys blur input to enter selection mode (don't move cursor yet)
			if key == "up" || key == "down" {
				m.input.Blur()
				return m, nil
			}

			return m.updateInput(msg)
		}

		// Input NOT focused - handle navigation and action keys
		switch key {
		case "q":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.servers)-1 {
				m.cursor++
			}
		case "i", "enter":
			if len(m.servers) > 0 && m.cursor < len(m.servers) {
				return m, m.installServer(m.cursor)
			}
		case "u":
			if len(m.servers) > 0 && m.cursor < len(m.servers) {
				return m, m.uninstallServer(m.cursor)
			}
		case "t":
			if len(m.servers) > 0 && m.cursor < len(m.servers) {
				m.testing = true
				return m, m.testServer(m.cursor)
			}
		}
		return m, nil

	case tea.PasteMsg:
		if m.input.Focused() {
			return m.updateInput(msg)
		}
		return m, nil

	case spinner.TickMsg:
		if m.loading || m.testing || m.searching {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	case debounceTickMsg:
		// Only trigger search if the query still matches current filter
		if msg.query == m.filterText && msg.query != m.lastSearchText {
			m.loading = true
			return m, m.loadServers(msg.query)
		}
		m.searching = false

	case searchResultMsg:
		m.loading = false
		m.searching = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			// Merge new servers with existing ones
			m.mergeServers(msg.servers)
			m.lastSearchText = msg.query
			m.err = nil
			m.applyFilter()
		}

	case installResultMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to install %s: %v", msg.name, msg.err)
		} else {
			m.message = fmt.Sprintf("Installed %s", msg.name)
			m.installed[msg.name] = true
			// Reload config
			if cfg, err := mcp.LoadConfig(); err == nil {
				m.config = cfg
			}
		}

	case uninstallResultMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to uninstall %s: %v", msg.name, msg.err)
		} else {
			m.message = fmt.Sprintf("Uninstalled %s", msg.name)
			delete(m.installed, msg.name)
			// Reload config
			if cfg, err := mcp.LoadConfig(); err == nil {
				m.config = cfg
				// Refresh installed map
				m.installed = make(map[string]bool)
				for name := range cfg.Servers {
					m.installed[name] = true
				}
			}
		}

	case testResultMsg:
		m.testing = false
		if msg.err != nil {
			m.message = fmt.Sprintf("Test failed: %v", msg.err)
		} else {
			m.message = fmt.Sprintf("%s has %d tools: %s", msg.name, len(msg.tools), strings.Join(msg.tools, ", "))
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) updateInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)

	if m.input.Value() != m.filterText {
		m.filterText = m.input.Value()
		m.applyFilter()

		if m.filterText != "" && m.filterText != m.lastSearchText {
			m.searching = true
			cmds = append(cmds, m.debounceSearch(m.filterText))
		}
	}

	return m, tea.Batch(cmds...)
}

// View renders the model
func (m *Model) View() tea.View {
	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("MCP Server Browser"))
	b.WriteString("\n\n")

	// Search input
	b.WriteString(m.input.View())
	b.WriteString("\n")

	// Status line (loading/searching indicator)
	if m.loading {
		b.WriteString(m.spinner.View())
		b.WriteString(" Searching registry...")
		b.WriteString("\n")
	} else if m.searching {
		b.WriteString(m.spinner.View())
		b.WriteString(" ...")
		b.WriteString("\n")
	} else if m.testing {
		b.WriteString(m.spinner.View())
		b.WriteString(" Testing server...")
		b.WriteString("\n")
	} else {
		// Empty line to maintain layout
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

	// Server list
	if len(m.servers) > 0 {
		// Calculate visible rows - each server takes 2 lines (name + description)
		// Reserve: title(2) + input(1) + status(1) + pagination(2) + help(2) = 8 lines
		reserved := 8
		if m.err != nil {
			reserved++
		}
		if m.message != "" {
			reserved++
		}
		availableHeight := ui.RemainingLines(m.height, reserved)

		// Each server entry is 2 lines
		visibleServers := availableHeight / 2
		if visibleServers < 2 {
			visibleServers = 2
		}

		startIdx, endIdx := ui.VisibleRange(len(m.servers), m.cursor, visibleServers)

		for i := startIdx; i < endIdx; i++ {
			s := m.servers[i].Server

			// Cursor - only show when input not focused (selection mode)
			cursor := "  "
			if i == m.cursor && !m.input.Focused() {
				cursor = selectedStyle.Render("> ")
			}
			b.WriteString(cursor)

			// Server name
			displayName := s.DisplayName()
			if i == m.cursor && !m.input.Focused() {
				b.WriteString(selectedStyle.Render(displayName))
			} else {
				b.WriteString(displayName)
			}

			// Installed badge
			if m.isServerInstalled(&s) {
				b.WriteString(" ")
				b.WriteString(installedStyle.Render("[installed]"))
			}
			b.WriteString("\n")

			// Description
			desc := s.Description
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

	} else if !m.loading && m.err == nil {
		if m.filterText != "" && len(m.allServers) > 0 {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("No servers match '%s'. Clear filter to see all %d servers.", m.filterText, len(m.allServers))))
		} else if m.filterText != "" {
			b.WriteString(mutedStyle.Render("Searching..."))
		} else {
			b.WriteString(mutedStyle.Render("No servers found."))
		}
		b.WriteString("\n")
	}

	// Help - show different hints based on focus state
	b.WriteString("\n")
	if m.input.Focused() {
		b.WriteString(helpStyle.Render("Type to filter • [↑↓] select • [Esc] quit"))
	} else {
		b.WriteString(helpStyle.Render("[Enter/i] install  [u] uninstall  [t] test  [Tab] filter  [q] quit"))
	}
	b.WriteString("\n")

	return ui.NewAltScreenView(b.String())
}

// debounceSearch returns a command that fires after a delay
func (m *Model) debounceSearch(query string) tea.Cmd {
	return tea.Tick(debounceDelay, func(t time.Time) tea.Msg {
		return debounceTickMsg{query: query}
	})
}

// loadServers loads servers from the registry
func (m *Model) loadServers(query string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		result, err := m.registry.Search(ctx, mcp.SearchOptions{
			Query: query,
			Limit: 200,
		})
		if err != nil {
			return searchResultMsg{err: err, query: query}
		}
		return searchResultMsg{servers: result.Servers, query: query}
	}
}

// mergeServers adds new servers to allServers, deduplicating by display name
func (m *Model) mergeServers(newServers []mcp.RegistryServerWrapper) {
	seen := make(map[string]bool)

	// Track existing servers
	for _, s := range m.allServers {
		seen[s.Server.DisplayName()] = true
	}

	// Add new servers that aren't duplicates
	for _, s := range newServers {
		name := s.Server.DisplayName()
		if !seen[name] {
			seen[name] = true
			m.allServers = append(m.allServers, s)
		}
	}
}

// applyFilter filters allServers based on filterText
func (m *Model) applyFilter() {
	if m.filterText == "" {
		m.servers = m.allServers
		m.cursor = 0
		return
	}

	filter := strings.ToLower(m.filterText)
	var filtered []mcp.RegistryServerWrapper
	for _, s := range m.allServers {
		name := strings.ToLower(s.Server.DisplayName())
		desc := strings.ToLower(s.Server.Description)
		if strings.Contains(name, filter) || strings.Contains(desc, filter) {
			filtered = append(filtered, s)
		}
	}
	m.servers = filtered
	if m.cursor >= len(m.servers) {
		m.cursor = 0
	}
}

// uninstallServer removes a server from the config
func (m *Model) uninstallServer(idx int) tea.Cmd {
	if idx >= len(m.servers) {
		return nil
	}

	server := m.servers[idx].Server
	displayName := server.DisplayName()

	// Check if installed
	if !m.isServerInstalled(&server) {
		return func() tea.Msg {
			return uninstallResultMsg{name: displayName, err: fmt.Errorf("not installed")}
		}
	}

	return func() tea.Msg {
		cfg, err := mcp.LoadConfig()
		if err != nil {
			return uninstallResultMsg{name: displayName, err: err}
		}

		// Find the installed name that matches
		var installedName string
		for name := range cfg.Servers {
			if strings.Contains(displayName, name) || strings.Contains(name, displayName) {
				installedName = name
				break
			}
		}

		if installedName == "" {
			return uninstallResultMsg{name: displayName, err: fmt.Errorf("not found in config")}
		}

		if !cfg.RemoveServer(installedName) {
			return uninstallResultMsg{name: installedName, err: fmt.Errorf("failed to remove")}
		}

		if err := cfg.Save(); err != nil {
			return uninstallResultMsg{name: installedName, err: err}
		}

		return uninstallResultMsg{name: installedName, err: nil}
	}
}

// installServer installs a server from the registry
func (m *Model) installServer(idx int) tea.Cmd {
	if idx >= len(m.servers) {
		return nil
	}

	server := m.servers[idx].Server

	return func() tea.Msg {
		// Convert to local config
		serverConfig, _ := server.ToServerConfig()
		if serverConfig.Command == "" {
			return installResultMsg{
				name: server.DisplayName(),
				err:  fmt.Errorf("no supported package found"),
			}
		}

		// Determine local name - prefer the server Name if it's clean (no @ or /)
		// Bundled servers have nice names like "discourse", "brave-search"
		localName := server.Name
		if localName == "" || strings.ContainsAny(localName, "@/") {
			// Fall back to deriving from package identifier
			localName = server.DisplayName()
			if strings.HasPrefix(localName, "@") {
				parts := strings.Split(localName, "/")
				if len(parts) > 1 {
					pkgName := parts[1]
					if pkgName == "mcp" {
						localName = strings.TrimPrefix(parts[0], "@")
					} else {
						localName = strings.TrimSuffix(pkgName, "-mcp")
						localName = strings.TrimPrefix(localName, "mcp-")
					}
				}
			}
		}

		// Load and update config
		cfg, err := mcp.LoadConfig()
		if err != nil {
			return installResultMsg{name: localName, err: err}
		}

		cfg.AddServer(localName, serverConfig)
		if err := cfg.Save(); err != nil {
			return installResultMsg{name: localName, err: err}
		}

		return installResultMsg{name: localName}
	}
}

// testServer tests a server connection
func (m *Model) testServer(idx int) tea.Cmd {
	if idx >= len(m.servers) {
		return nil
	}

	server := m.servers[idx].Server

	return func() tea.Msg {
		displayName := server.DisplayName()

		// Convert to local config
		serverConfig, _ := server.ToServerConfig()
		if serverConfig.Command == "" {
			return testResultMsg{
				name: displayName,
				err:  fmt.Errorf("no supported package found"),
			}
		}

		// Create client and test
		client := mcp.NewClient(displayName, serverConfig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := client.Start(ctx); err != nil {
			return testResultMsg{name: displayName, err: err}
		}
		defer client.Stop()

		tools := client.Tools()
		var toolNames []string
		for _, t := range tools {
			toolNames = append(toolNames, t.Name)
		}

		return testResultMsg{name: displayName, tools: toolNames}
	}
}

// isInstalled checks if a server is already installed
// Takes the full RegistryServer to access both Name and DisplayName
func (m *Model) isServerInstalled(server *mcp.RegistryServer) bool {
	// This must match the logic in installServer
	localName := server.Name
	if localName == "" || strings.ContainsAny(localName, "@/") {
		localName = server.DisplayName()
		if strings.HasPrefix(localName, "@") {
			parts := strings.Split(localName, "/")
			if len(parts) > 1 {
				pkgName := parts[1]
				if pkgName == "mcp" {
					localName = strings.TrimPrefix(parts[0], "@")
				} else {
					localName = strings.TrimSuffix(pkgName, "-mcp")
					localName = strings.TrimPrefix(localName, "mcp-")
				}
			}
		}
	}

	return m.installed[localName]
}

// RunBrowser runs the MCP browser TUI
func RunBrowser(initialQuery string) error {
	model := New(initialQuery)
	p := tea.NewProgram(model)
	_, err := p.Run()
	return err
}
