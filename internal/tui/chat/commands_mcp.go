package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
)

func (m *Model) cmdMcp(args []string) (tea.Model, tea.Cmd) {
	// No args - open the MCP picker dialog
	if len(args) == 0 {
		if m.mcpManager == nil || len(m.mcpManager.AvailableServers()) == 0 {
			return m.showMCPQuickStart()
		}
		m.showMCPPicker()
		m.setTextareaValue("")
		return m, nil
	}

	subCmd := strings.ToLower(args[0])
	subArgs := args[1:]

	switch subCmd {
	case "list":
		return m.showBundledServersList()

	case "add":
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/mcp add <server>`\n\nUse `/mcp list` to see available servers.")
		}
		return m.quickAddMCP(strings.Join(subArgs, " "))

	case "start":
		if m.mcpManager == nil {
			return m.showMCPQuickStart()
		}
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/mcp start <server>`\n\nUse `/mcp` to see configured servers.")
		}
		return m.mcpStartServer(strings.Join(subArgs, " "))

	case "stop":
		if m.mcpManager == nil {
			return m.showSystemMessage("No MCP servers configured.")
		}
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/mcp stop <server>`\n\nUse `/mcp` to see running servers.")
		}
		return m.mcpStopServer(strings.Join(subArgs, " "))

	case "restart":
		if m.mcpManager == nil {
			return m.showSystemMessage("No MCP servers configured.")
		}
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/mcp restart <server>`")
		}
		name, err := m.mcpFindServer(subArgs[0])
		if err != nil {
			return m.showSystemMessage(err.Error())
		}
		if err := m.mcpManager.Restart(context.Background(), name); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to restart %s: %v", name, err))
		}
		m.setMCPServerSelected(name, true)
		m.setTextareaValue("")
		return m.showSystemMessage(fmt.Sprintf("Restarting MCP server: %s", name))

	case "login":
		if m.mcpManager == nil {
			return m.showMCPQuickStart()
		}
		if len(subArgs) != 1 {
			return m.showSystemMessage("Usage: `/mcp login <server>`")
		}
		name, err := m.mcpFindServer(subArgs[0])
		if err != nil {
			return m.showSystemMessage(err.Error())
		}
		m.setTextareaValue("")
		_, footerCmd := m.showFooterMessage("Waiting for MCP authorization in your browser…")
		return m, tea.Batch(footerCmd, m.startMCPOAuthCmd(name, false))

	case "logout":
		if m.mcpManager == nil {
			return m.showMCPQuickStart()
		}
		if len(subArgs) != 1 {
			return m.showSystemMessage("Usage: `/mcp logout <server>`")
		}
		name, err := m.mcpFindServer(subArgs[0])
		if err != nil {
			return m.showSystemMessage(err.Error())
		}
		m.setTextareaValue("")
		return m, m.logoutMCPOAuthCmd(name)

	case "status":
		if m.mcpManager == nil {
			return m.showMCPQuickStart()
		}
		showTools := len(subArgs) > 0 && strings.EqualFold(subArgs[0], "tools")
		if len(subArgs) > 0 && !showTools {
			return m.showSystemMessage("Usage: `/mcp status [tools]`")
		}
		return m.mcpShowStatus(showTools)

	default:
		return m.showSystemMessage(fmt.Sprintf("Unknown subcommand: %s\n\n**Commands:**\n- `/mcp start <server>` - Start a server\n- `/mcp stop <server>` - Stop a server\n- `/mcp add <server>` - Add a new server\n- `/mcp list` - Show available servers\n- `/mcp status` - Show current status", subCmd))
	}
}

// mcpFindServer finds a server by fuzzy matching
func (m *Model) mcpFindServer(query string) (string, error) {
	available := m.mcpManager.AvailableServers()
	if len(available) == 0 {
		return "", fmt.Errorf("no MCP servers configured\n\nUse `/mcp add %s` to add it", query)
	}

	queryLower := strings.ToLower(query)
	var exactMatch string
	var prefixMatches []string
	var containsMatches []string

	for _, s := range available {
		sLower := strings.ToLower(s)
		if sLower == queryLower {
			exactMatch = s
			break
		}
		if strings.HasPrefix(sLower, queryLower) {
			prefixMatches = append(prefixMatches, s)
		} else if strings.Contains(sLower, queryLower) {
			containsMatches = append(containsMatches, s)
		}
	}

	if exactMatch != "" {
		return exactMatch, nil
	}
	if len(prefixMatches) == 1 {
		return prefixMatches[0], nil
	}
	if len(prefixMatches) > 1 {
		return "", fmt.Errorf("multiple servers match '%s':\n\n- %s\n\nBe more specific", query, strings.Join(prefixMatches, "\n- "))
	}
	if len(containsMatches) == 1 {
		return containsMatches[0], nil
	}
	if len(containsMatches) > 1 {
		return "", fmt.Errorf("multiple servers match '%s':\n\n- %s\n\nBe more specific", query, strings.Join(containsMatches, "\n- "))
	}
	return "", fmt.Errorf("no server matches '%s'\n\nConfigured: %s", query, strings.Join(available, ", "))
}

// mcpStartServer starts a server by name (with fuzzy matching)
func (m *Model) mcpStartServer(query string) (tea.Model, tea.Cmd) {
	name, err := m.mcpFindServer(query)
	if err != nil {
		return m.showSystemMessage(err.Error())
	}

	status, _ := m.mcpManager.ServerStatus(name)
	if status == "ready" {
		return m.showSystemMessage(fmt.Sprintf("Server '%s' is already running.", name))
	}
	if status == "starting" {
		return m.showSystemMessage(fmt.Sprintf("Server '%s' is already starting.", name))
	}

	if err := m.mcpManager.Enable(context.Background(), name); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to start %s: %v", name, err))
	}
	m.setMCPServerSelected(name, true)
	m.setTextareaValue("")
	return m.showFooterMuted(fmt.Sprintf("Starting %s… tools will be available shortly.", name))
}

// mcpStopServer stops a server by name (with fuzzy matching)
func (m *Model) mcpStopServer(query string) (tea.Model, tea.Cmd) {
	name, err := m.mcpFindServer(query)
	if err != nil {
		return m.showSystemMessage(err.Error())
	}

	status, _ := m.mcpManager.ServerStatus(name)
	if status == "stopped" || status == "" {
		return m.showSystemMessage(fmt.Sprintf("Server '%s' is not running.", name))
	}

	if err := m.mcpManager.Disable(name); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to stop %s: %v", name, err))
	}
	m.setMCPServerSelected(name, false)
	m.setTextareaValue("")
	return m.showFooterSuccess(fmt.Sprintf("Stopped %s.", name))
}

func formatMCPFailureMessage(update mcp.StatusUpdate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## MCP server failed\n\n`%s` failed.\n", update.Name)
	if update.Error != nil {
		detail := strings.TrimSpace(update.Error.Error())
		if detail != "" {
			b.WriteString("\n**Error:**\n\n> ")
			b.WriteString(strings.ReplaceAll(detail, "\n", "\n> "))
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "\nThe error remains available in `/mcp`. To retry outside chat, run `term-llm mcp info %s`.\n", update.Name)
	return b.String()
}

func (m *Model) mcpShowStatus(showTools bool) (tea.Model, tea.Cmd) {
	var b strings.Builder

	available := m.mcpManager.AvailableServers()
	states := m.mcpManager.GetAllStates()

	if len(available) == 0 {
		b.WriteString("No MCP servers configured.\n\n")
		b.WriteString("Quick start:\n")
		b.WriteString("  /mcp add playwright  Browser automation\n")
		b.WriteString("  /mcp add github      GitHub integration\n")
		b.WriteString("  /mcp add filesystem  File operations\n")
		b.WriteString("  /mcp list            See all available servers")
		return m.showMCPStatusContent(b.String())
	}

	stateMap := make(map[string]mcp.ServerState, len(states))
	for _, state := range states {
		stateMap[state.Name] = state
	}

	activeCount := 0
	stoppedCount := 0
	for _, name := range available {
		state := stateMap[name]
		if state.Status == "" || state.Status == mcp.StatusStopped {
			stoppedCount++
			continue
		}
		activeCount++
		fmt.Fprintf(&b, "%s: %s", name, state.Status)
		if state.Status == mcp.StatusReady {
			fmt.Fprintf(&b, " (%d catalogued tools)", state.ToolCount)
		}
		if state.Error != nil {
			fmt.Fprintf(&b, "\n  error: %v", state.Error)
		}
		if state.RefreshError != nil {
			fmt.Fprintf(&b, "\n  refresh warning: %v", state.RefreshError)
		}
		if !state.LastToolRefresh.IsZero() {
			fmt.Fprintf(&b, "\n  refreshed: %s", state.LastToolRefresh.Local().Format(time.RFC3339))
		}
		b.WriteString("\n")
	}
	if activeCount == 0 {
		b.WriteString("No MCP servers are running.\n")
	}
	if stoppedCount > 0 {
		fmt.Fprintf(&b, "%d other configured servers stopped.\n", stoppedCount)
	}

	tools := m.mcpManager.AllTools()
	m.writeMCPModelAccessStatus(&b, len(tools))
	if showTools && len(tools) > 0 {
		fmt.Fprintf(&b, "\nCatalogued tools (%d):\n", len(tools))
		for _, tool := range tools {
			fmt.Fprintf(&b, "  %s\n", tool.Name)
		}
	} else if len(tools) > 0 {
		b.WriteString("\nRun /mcp status tools to list tool names.")
	}

	return m.showMCPStatusContent(b.String())
}

func (m *Model) writeMCPModelAccessStatus(b *strings.Builder, catalogueCount int) {
	if b == nil {
		return
	}
	b.WriteString("\nModel access: ")
	if m.engine == nil {
		b.WriteString("unavailable (no active engine)\n")
		return
	}
	sessionID := ""
	if m.sess != nil {
		sessionID = m.sess.ID
	}
	diagnostics, ok := m.engine.ToolDiscoveryDiagnostics(sessionID)
	snapshot := m.mcpManager.CatalogueSnapshot()
	if !ok || diagnostics.ResolvedMode == "" || mcpDiscoveryDiagnosticsStale(diagnostics, snapshot) {
		if catalogueCount == 0 {
			b.WriteString("no catalogued tools\n")
		} else {
			b.WriteString("pending; send a new message now that the server is ready\n")
		}
		return
	}
	authorized := diagnostics.PinnedCount + diagnostics.ActiveMCPCount + diagnostics.DeferredCount
	if catalogueCount > 0 && authorized == 0 {
		b.WriteString("blocked by the active tool policy\n")
		return
	}
	switch diagnostics.ResolvedMode {
	case "deferred":
		control := "tool_search"
		if diagnostics.Strategy == "native" {
			control = "native tool search"
		}
		fmt.Fprintf(b, "%d active, %d deferred via %s\n", diagnostics.PinnedCount+diagnostics.ActiveMCPCount, diagnostics.DeferredCount, control)
	case "eager":
		fmt.Fprintf(b, "%d tools included directly\n", authorized)
	default:
		fmt.Fprintf(b, "%d tools authorised (%s mode)\n", authorized, diagnostics.ResolvedMode)
	}
}

func mcpDiscoveryDiagnosticsStale(diagnostics llm.ToolDiscoveryDiagnostics, snapshot *mcp.CatalogueSnapshot) bool {
	if snapshot == nil || len(snapshot.Tools) == 0 {
		return false
	}
	return diagnostics.CatalogueHash == "" || diagnostics.CatalogueHash != snapshot.Hash
}

func (m *Model) showMCPStatusContent(content string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	m.clearFooterMessage()
	m.dialog.ShowContent("MCP Status", content)
	return m, nil
}

// showMCPQuickStart shows helpful info when user presses Ctrl+M with no MCPs configured
func (m *Model) showMCPQuickStart() (tea.Model, tea.Cmd) {
	var b strings.Builder
	b.WriteString("## MCP Quick Start\n\n")
	b.WriteString("MCP servers give the LLM tools like browser automation, database access, and more.\n\n")
	b.WriteString("**Popular servers:**\n")
	b.WriteString("- `playwright` - Browser automation with accessibility snapshots\n")
	b.WriteString("- `filesystem` - Secure file operations\n")
	b.WriteString("- `git` - Git repository operations\n")
	b.WriteString("- `github` - GitHub API integration\n")
	b.WriteString("- `fetch` - Web content fetching\n")
	b.WriteString("\n**Get started:**\n")
	b.WriteString("- `/mcp add playwright` - Add a server\n")
	b.WriteString("- `/mcp list` - See all available servers\n")

	m.setTextareaValue("")
	return m.showSystemMessage(b.String())
}

// quickAddMCP adds an MCP server from bundled servers
func (m *Model) quickAddMCP(query string) (tea.Model, tea.Cmd) {
	bundled := mcp.GetBundledServers()
	queryLower := strings.ToLower(query)

	// First try exact name match
	var match *mcp.BundledServer
	for i, s := range bundled {
		if strings.ToLower(s.Name) == queryLower {
			match = &bundled[i]
			break
		}
	}

	// Then try fuzzy match on name or description
	if match == nil {
		for i, s := range bundled {
			if strings.Contains(strings.ToLower(s.Name), queryLower) ||
				strings.Contains(strings.ToLower(s.Description), queryLower) {
				match = &bundled[i]
				break
			}
		}
	}

	if match == nil {
		return m.showSystemMessage(fmt.Sprintf(
			"No server found matching '%s'.\n\nUse `/mcp list` to see available servers.",
			query))
	}

	// Load config
	cfg, err := mcp.LoadConfig()
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to load MCP config: %v", err))
	}

	// Check if already configured
	if _, exists := cfg.Servers[match.Name]; exists {
		return m.showSystemMessage(fmt.Sprintf("Server '%s' is already configured.\n\nUse `/mcp %s` to enable it.", match.Name, match.Name))
	}

	// Add to config
	serverConfig := match.ToServerConfig()
	if cfg.Servers == nil {
		cfg.Servers = make(map[string]mcp.ServerConfig)
	}
	cfg.Servers[match.Name] = serverConfig

	if err := cfg.Save(); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to save MCP config: %v", err))
	}

	// Reload manager config and auto-enable the server
	if m.mcpManager != nil {
		if err := m.mcpManager.LoadConfig(); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Added '%s' but failed to reload config: %v\n\nRestart chat to use it.", match.Name, err))
		}
		// Auto-enable the newly added server
		if err := m.mcpManager.Enable(context.Background(), match.Name); err != nil {
			m.setTextareaValue("")
			return m.showSystemMessage(fmt.Sprintf(
				"Added **%s** but failed to start: %v\n\nUse `/mcp %s` to try again.",
				match.Name, err, match.Name))
		}
	}

	m.setMCPServerSelected(match.Name, true)
	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf(
		"Enabled **%s**\n\n%s\n\nTools will be available shortly.",
		match.Name, match.Description))
}

// showBundledServersList shows available bundled servers
func (m *Model) showBundledServersList() (tea.Model, tea.Cmd) {
	bundled := mcp.GetBundledServers()

	// Group by category
	byCategory := make(map[string][]mcp.BundledServer)
	for _, s := range bundled {
		byCategory[s.Category] = append(byCategory[s.Category], s)
	}

	// Define category order
	categoryOrder := []string{"Reference", "Browser", "DevTools", "Database", "Cloud", "Productivity", "Search", "Data", "Finance", "Communication", "Other"}

	var b strings.Builder
	b.WriteString("## Available MCP Servers\n\n")

	for _, cat := range categoryOrder {
		servers, ok := byCategory[cat]
		if !ok || len(servers) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("**%s:**\n", cat))
		for _, s := range servers {
			b.WriteString(fmt.Sprintf("- `%s` - %s\n", s.Name, s.Description))
		}
		b.WriteString("\n")
	}

	b.WriteString("Use `/mcp add <name>` to add a server.\n")

	m.setTextareaValue("")
	return m.showSystemMessage(b.String())
}
