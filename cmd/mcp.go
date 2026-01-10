package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/mcp"
	mcpTui "github.com/samsaffron/term-llm/internal/tui/mcp"
	"github.com/spf13/cobra"
)

var mcpBrowseTUI bool

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Manage MCP (Model Context Protocol) servers",
	Long: `Manage MCP servers for extending term-llm with external tools.

MCP servers provide additional capabilities like browser automation,
filesystem access, and more.

Examples:
  term-llm mcp list                    # list configured servers
  term-llm mcp browse playwright       # search registry for playwright
  term-llm mcp add @playwright/mcp     # add server from registry
  term-llm mcp remove playwright       # remove a server
  term-llm mcp test playwright         # test server connection`,
}

var mcpListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured MCP servers",
	RunE:  mcpList,
}

var mcpBrowseCmd = &cobra.Command{
	Use:   "browse [search]",
	Short: "Browse the MCP registry",
	Long: `Search and browse the official MCP registry.

Examples:
  term-llm mcp browse                  # open interactive browser
  term-llm mcp browse playwright       # search for playwright
  term-llm mcp browse --no-tui         # simple CLI output`,
	RunE: mcpBrowse,
}

var mcpAddCmd = &cobra.Command{
	Use:   "add <name-or-url>",
	Short: "Add an MCP server from the registry or URL",
	Long: `Add an MCP server by searching the registry or connecting to a URL.

The argument can be:
  - A URL like https://example.com/mcp (HTTP transport)
  - A package name like @playwright/mcp
  - A search term like playwright

Examples:
  term-llm mcp add https://developers.openai.com/mcp
  term-llm mcp add @playwright/mcp
  term-llm mcp add playwright`,
	Args: cobra.ExactArgs(1),
	RunE: mcpAdd,
}

var mcpRemoveCmd = &cobra.Command{
	Use:               "remove <name>",
	Short:             "Remove an MCP server",
	Args:              cobra.ExactArgs(1),
	RunE:              mcpRemove,
	ValidArgsFunction: MCPServerArgCompletion,
}

var mcpTestCmd = &cobra.Command{
	Use:   "test <name>",
	Short: "Test an MCP server connection",
	Long: `Start an MCP server and verify it responds correctly.

This will:
  1. Start the server process
  2. Send an initialization request
  3. List available tools
  4. Stop the server

Examples:
  term-llm mcp test playwright`,
	Args:              cobra.ExactArgs(1),
	RunE:              mcpTest,
	ValidArgsFunction: MCPServerArgCompletion,
}

var mcpPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print MCP configuration file path",
	RunE:  mcpPath,
}

func init() {
	mcpBrowseCmd.Flags().BoolVar(&mcpBrowseTUI, "no-tui", false, "Use simple CLI output instead of interactive browser")
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.AddCommand(mcpListCmd)
	mcpCmd.AddCommand(mcpBrowseCmd)
	mcpCmd.AddCommand(mcpAddCmd)
	mcpCmd.AddCommand(mcpRemoveCmd)
	mcpCmd.AddCommand(mcpTestCmd)
	mcpCmd.AddCommand(mcpPathCmd)
}

func mcpList(cmd *cobra.Command, args []string) error {
	cfg, err := mcp.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if len(cfg.Servers) == 0 {
		fmt.Println("No MCP servers configured.")
		fmt.Println()
		fmt.Println("Add one with: term-llm mcp add <name>")
		fmt.Println("Browse available servers: term-llm mcp browse")
		return nil
	}

	fmt.Printf("Configured MCP servers (%d):\n\n", len(cfg.Servers))
	for name, server := range cfg.Servers {
		fmt.Printf("  %s\n", name)
		if server.TransportType() == "http" {
			fmt.Printf("    url: %s\n", server.URL)
			if len(server.Headers) > 0 {
				fmt.Printf("    headers: %d configured\n", len(server.Headers))
			}
		} else {
			fmt.Printf("    command: %s %s\n", server.Command, strings.Join(server.Args, " "))
		}
		if len(server.Env) > 0 {
			fmt.Printf("    env: %d variables\n", len(server.Env))
		}
	}

	path, _ := mcp.DefaultConfigPath()
	fmt.Printf("\nConfig file: %s\n", path)
	return nil
}

func mcpBrowse(cmd *cobra.Command, args []string) error {
	query := ""
	if len(args) > 0 {
		query = args[0]
	}

	// Use interactive TUI by default
	if !mcpBrowseTUI {
		return mcpTui.RunBrowser(query)
	}

	// Fallback to simple CLI output
	seen := make(map[string]bool)
	var servers []mcp.RegistryServerWrapper
	queryLower := strings.ToLower(query)

	// Add bundled servers first (curated top MCPs), filtered by query
	for _, wrapper := range mcp.GetBundledAsRegistryWrappers() {
		name := wrapper.Server.DisplayName()
		// Filter bundled servers by query if provided
		if query != "" {
			nameLower := strings.ToLower(name)
			descLower := strings.ToLower(wrapper.Server.Description)
			if !strings.Contains(nameLower, queryLower) && !strings.Contains(descLower, queryLower) {
				continue
			}
		}
		if !seen[name] {
			seen[name] = true
			servers = append(servers, wrapper)
		}
	}

	// Only search registry if a query is provided
	if query != "" {
		fmt.Printf("Searching MCP registry for '%s'...\n\n", query)

		registry := mcp.NewRegistryClient()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		result, err := registry.Search(ctx, mcp.SearchOptions{
			Query: query,
			Limit: 100,
		})
		if err != nil {
			// Don't fail - just show bundled results
			fmt.Printf("(registry unavailable: %v)\n\n", err)
		} else {
			// Add registry servers (dedupe against bundled)
			for _, wrapper := range result.Servers {
				name := wrapper.Server.DisplayName()
				if !seen[name] {
					seen[name] = true
					servers = append(servers, wrapper)
				}
			}
		}
	} else {
		fmt.Println("Curated MCP servers (type a query to search registry):")
	}

	if len(servers) == 0 {
		fmt.Println("No servers found.")
		return nil
	}

	// Load local config to show installed status
	cfg, _ := mcp.LoadConfig()

	for _, wrapper := range servers {
		s := wrapper.Server
		displayName := s.DisplayName()

		// Check if installed
		installed := false
		if cfg != nil {
			for name := range cfg.Servers {
				if strings.Contains(name, displayName) || strings.Contains(displayName, name) {
					installed = true
					break
				}
			}
		}

		status := ""
		if installed {
			status = " [installed]"
		}

		fmt.Printf("  %s%s\n", displayName, status)
		if s.Description != "" {
			// Truncate long descriptions
			desc := s.Description
			if len(desc) > 70 {
				desc = desc[:67] + "..."
			}
			fmt.Printf("    %s\n", desc)
		}

		// Show install command hint
		if !installed && len(s.Packages) > 0 {
			for _, pkg := range s.Packages {
				if pkg.RegistryType == "npm" {
					fmt.Printf("    install: term-llm mcp add %s\n", pkg.Identifier)
					break
				}
			}
		}
		fmt.Println()
	}

	fmt.Printf("Found %d servers\n", len(servers))

	return nil
}

func mcpAdd(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Check if it's a URL
	if strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://") {
		return mcpAddURL(name)
	}

	// Otherwise, search the registry
	return mcpAddFromRegistry(name)
}

// mcpAddURL adds an MCP server from a URL (HTTP transport).
func mcpAddURL(urlStr string) error {
	// Parse and validate the URL
	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must use http or https scheme")
	}

	// Derive a local name from the URL
	// Use hostname, stripping common prefixes/suffixes
	localName := u.Hostname()
	localName = strings.TrimPrefix(localName, "www.")
	localName = strings.TrimPrefix(localName, "api.")
	localName = strings.TrimPrefix(localName, "mcp.")

	// Remove common TLDs and suffixes for cleaner names
	localName = strings.TrimSuffix(localName, ".com")
	localName = strings.TrimSuffix(localName, ".io")
	localName = strings.TrimSuffix(localName, ".ai")
	localName = strings.TrimSuffix(localName, ".dev")

	// Replace dots with hyphens
	localName = strings.ReplaceAll(localName, ".", "-")

	// If the path has a meaningful segment, append it
	pathParts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(pathParts) > 0 && pathParts[0] != "" && pathParts[0] != "mcp" {
		localName = localName + "-" + pathParts[0]
	}

	fmt.Printf("Adding MCP server from URL: %s\n", urlStr)
	fmt.Printf("  name: %s\n", localName)
	fmt.Printf("  transport: http (streamable)\n")
	fmt.Println()

	// Create the server config
	serverConfig := mcp.ServerConfig{
		Type: "http",
		URL:  urlStr,
	}

	// Load and update config
	cfg, err := mcp.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if _, exists := cfg.Servers[localName]; exists {
		return fmt.Errorf("server '%s' already exists in config", localName)
	}

	cfg.AddServer(localName, serverConfig)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	path, _ := mcp.DefaultConfigPath()
	fmt.Printf("Added '%s' to %s\n", localName, path)
	fmt.Println()
	fmt.Printf("Test it with: term-llm mcp test %s\n", localName)
	fmt.Printf("Use with: term-llm [ask|exec|edit|chat] --mcp %s ...\n", localName)

	return nil
}

// mcpAddFromRegistry adds an MCP server from bundled list or registry.
func mcpAddFromRegistry(name string) error {
	nameLower := strings.ToLower(name)

	// First, check bundled servers (curated list takes priority)
	for _, bundled := range mcp.GetBundledServers() {
		if strings.ToLower(bundled.Name) == nameLower ||
			strings.ToLower(bundled.Package) == nameLower ||
			strings.HasSuffix(strings.ToLower(bundled.Package), "/"+nameLower) {
			return addBundledServer(bundled)
		}
	}

	// Not in bundled list, search the registry
	registry := mcp.NewRegistryClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("Searching registry for '%s'...\n", name)

	result, err := registry.Search(ctx, mcp.SearchOptions{
		Query: name,
		Limit: 50,
	})
	if err != nil {
		return fmt.Errorf("search registry: %w", err)
	}

	if len(result.Servers) == 0 {
		return fmt.Errorf("no servers found matching '%s'", name)
	}

	// Find best match - prefer exact package name match
	var bestMatch *mcp.RegistryServer
	for i := range result.Servers {
		s := &result.Servers[i].Server
		for _, pkg := range s.Packages {
			if pkg.Identifier == name || strings.HasSuffix(pkg.Identifier, "/"+name) {
				bestMatch = s
				break
			}
		}
		if bestMatch != nil {
			break
		}
	}

	// Fall back to first result
	if bestMatch == nil {
		bestMatch = &result.Servers[0].Server
	}

	fmt.Printf("Found: %s\n", bestMatch.DisplayName())
	if bestMatch.Description != "" {
		fmt.Printf("  %s\n", bestMatch.Description)
	}
	fmt.Println()

	// Convert to local config
	serverConfig, needsInput := bestMatch.ToServerConfig()
	if serverConfig.Command == "" {
		return fmt.Errorf("no supported package found for %s (requires npm or pypi)", bestMatch.Name)
	}

	if needsInput {
		fmt.Println("This server requires configuration:")
		fmt.Printf("  command: %s %s\n", serverConfig.Command, strings.Join(serverConfig.Args, " "))
		fmt.Println()
		fmt.Println("Please edit mcp.json to fill in required arguments marked with <>")
	}

	// Determine local name - prefer server Name if it's clean (no @ or /)
	// Registry servers often have nice names like "discourse", "brave-search"
	localName := bestMatch.Name
	if localName == "" || strings.ContainsAny(localName, "@/") {
		// Fall back to deriving from the user-provided name or package identifier
		localName = name
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
		return fmt.Errorf("load config: %w", err)
	}

	if _, exists := cfg.Servers[localName]; exists {
		return fmt.Errorf("server '%s' already exists in config", localName)
	}

	cfg.AddServer(localName, serverConfig)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	path, _ := mcp.DefaultConfigPath()
	fmt.Printf("Added '%s' to %s\n", localName, path)
	fmt.Println()
	fmt.Printf("Test it with: term-llm mcp test %s\n", localName)
	fmt.Printf("Use with: term-llm [ask|exec|edit|chat] --mcp %s ...\n", localName)

	return nil
}

// addBundledServer adds a server from the bundled list.
func addBundledServer(bundled mcp.BundledServer) error {
	fmt.Printf("Found: %s (bundled)\n", bundled.Name)
	if bundled.Description != "" {
		fmt.Printf("  %s\n", bundled.Description)
	}
	fmt.Println()

	serverConfig := bundled.ToServerConfig()
	localName := bundled.Name

	// Load and update config
	cfg, err := mcp.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if _, exists := cfg.Servers[localName]; exists {
		return fmt.Errorf("server '%s' already exists in config", localName)
	}

	cfg.AddServer(localName, serverConfig)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	path, _ := mcp.DefaultConfigPath()
	fmt.Printf("Added '%s' to %s\n", localName, path)
	fmt.Println()
	fmt.Printf("Test it with: term-llm mcp test %s\n", localName)
	fmt.Printf("Use with: term-llm [ask|exec|edit|chat] --mcp %s ...\n", localName)

	return nil
}

func mcpRemove(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfg, err := mcp.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if !cfg.RemoveServer(name) {
		return fmt.Errorf("server '%s' not found in config", name)
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("Removed '%s' from config\n", name)
	return nil
}

func mcpTest(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfg, err := mcp.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	serverCfg, ok := cfg.Servers[name]
	if !ok {
		return fmt.Errorf("server '%s' not found in config", name)
	}

	fmt.Printf("Testing MCP server '%s'...\n", name)
	if serverCfg.TransportType() == "http" {
		fmt.Printf("  url: %s\n", serverCfg.URL)
	} else {
		fmt.Printf("  command: %s %s\n", serverCfg.Command, strings.Join(serverCfg.Args, " "))
	}
	fmt.Println()

	client := mcp.NewClient(name, serverCfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if serverCfg.TransportType() == "http" {
		fmt.Print("Connecting to server...")
	} else {
		fmt.Print("Starting server...")
	}
	if err := client.Start(ctx); err != nil {
		fmt.Println(" FAILED")
		return fmt.Errorf("connect to server: %w", err)
	}
	fmt.Println(" OK")
	defer client.Stop()

	tools := client.Tools()
	fmt.Printf("\nAvailable tools (%d):\n", len(tools))
	for _, t := range tools {
		fmt.Printf("  - %s\n", t.Name)
		if t.Description != "" {
			desc := t.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Printf("    %s\n", desc)
		}
	}

	fmt.Println()
	fmt.Printf("Server '%s' is working correctly.\n", name)
	return nil
}

func mcpPath(cmd *cobra.Command, args []string) error {
	path, err := mcp.DefaultConfigPath()
	if err != nil {
		return err
	}

	// Check if file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("%s (not created yet)\n", path)
	} else {
		fmt.Println(path)
	}
	return nil
}
