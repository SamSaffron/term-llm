package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
)

var (
	agentsBuiltin bool
	agentsLocal   bool
	agentsUser    bool
)

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Manage agents (named configuration bundles)",
	Long: `List and manage agents for term-llm.

Agents are named configuration bundles that combine system prompts,
tool sets, model preferences, and MCP servers.

Examples:
  term-llm agents                    # List all available agents
  term-llm agents --builtin          # Only built-in agents
  term-llm agents --local            # Only project-local agents
  term-llm agents new my-agent       # Create a new agent from template
  term-llm agents show reviewer      # Display agent configuration
  term-llm agents edit my-agent      # Open agent in $EDITOR
  term-llm agents copy reviewer my-reviewer  # Copy for customization`,
	RunE: runAgentsList,
}

var agentsNewCmd = &cobra.Command{
	Use:   "new <name>",
	Short: "Create a new agent from template",
	Long: `Create a new agent directory with template files.

By default, creates the agent in the user's config directory
(~/.config/term-llm/agents/). Use --local to create in the
current project's term-llm-agents/ directory.

Examples:
  term-llm agents new my-agent        # Create in user config
  term-llm agents new my-agent --local # Create in project`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentsNew,
}

var agentsShowCmd = &cobra.Command{
	Use:               "show <name>",
	Short:             "Display agent configuration",
	Args:              cobra.ExactArgs(1),
	RunE:              runAgentsShow,
	ValidArgsFunction: agentNameCompletion,
}

var agentsEditCmd = &cobra.Command{
	Use:               "edit <name>",
	Short:             "Open agent in $EDITOR",
	Args:              cobra.ExactArgs(1),
	RunE:              runAgentsEdit,
	ValidArgsFunction: agentNameCompletion,
}

var agentsCopyCmd = &cobra.Command{
	Use:   "copy <source> <dest>",
	Short: "Copy an agent for customization",
	Long: `Copy an existing agent to create a customized version.

This is useful for creating modified versions of built-in agents.

Examples:
  term-llm agents copy reviewer my-reviewer
  term-llm agents copy commit detailed-commit`,
	Args: cobra.ExactArgs(2),
	RunE: runAgentsCopy,
}

var agentsPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print agent directories",
	RunE:  runAgentsPath,
}

var agentsPrefSetCmd = &cobra.Command{
	Use:   "set <agent> key=value [key=value...]",
	Short: "Set preferences for an agent",
	Long: `Set preferences that override agent settings.

Preferences are stored in your config file and applied on top of
the agent's built-in configuration. This lets you customize agents
without copying them.

Valid preference keys:
  provider, model           - Override LLM provider/model
  max_turns                 - Override max conversation turns
  search                    - Enable/disable web search (true/false)
  tools_enabled             - Comma-separated list of enabled tools
  tools_disabled            - Comma-separated list of disabled tools
  shell_allow               - Comma-separated shell patterns to allow
  shell_auto_run            - Auto-approve shell commands (true/false)
  spawn_max_parallel        - Max parallel sub-agents
  spawn_max_depth           - Max spawn nesting depth
  spawn_timeout             - Spawn timeout in seconds
  spawn_allowed_agents      - Comma-separated list of allowed agents

Examples:
  term-llm agents set reviewer provider=gemini model=gemini-2.5-pro
  term-llm agents set developer max_turns=50
  term-llm agents set codebase search=true`,
	Args:              cobra.MinimumNArgs(2),
	RunE:              runAgentsPrefSet,
	ValidArgsFunction: agentPrefSetCompletion,
}

var agentsPrefGetCmd = &cobra.Command{
	Use:               "get <agent>",
	Short:             "Show preferences for an agent",
	Args:              cobra.ExactArgs(1),
	RunE:              runAgentsPrefGet,
	ValidArgsFunction: agentNameCompletion,
}

var agentsPrefClearCmd = &cobra.Command{
	Use:               "clear <agent>",
	Short:             "Clear all preferences for an agent",
	Args:              cobra.ExactArgs(1),
	RunE:              runAgentsPrefClear,
	ValidArgsFunction: agentNameCompletion,
}

func init() {
	agentsCmd.Flags().BoolVar(&agentsBuiltin, "builtin", false, "Show only built-in agents")
	agentsCmd.Flags().BoolVar(&agentsLocal, "local", false, "Show only project-local agents")
	agentsCmd.Flags().BoolVar(&agentsUser, "user", false, "Show only user-global agents")
	agentsNewCmd.Flags().BoolVar(&agentsLocal, "local", false, "Create in project's term-llm-agents/ instead of user config")
	agentsCopyCmd.Flags().BoolVar(&agentsLocal, "local", false, "Copy to project's term-llm-agents/ instead of user config")

	rootCmd.AddCommand(agentsCmd)
	agentsCmd.AddCommand(agentsNewCmd)
	agentsCmd.AddCommand(agentsShowCmd)
	agentsCmd.AddCommand(agentsEditCmd)
	agentsCmd.AddCommand(agentsCopyCmd)
	agentsCmd.AddCommand(agentsPathCmd)
	agentsCmd.AddCommand(agentsPrefSetCmd)
	agentsCmd.AddCommand(agentsPrefGetCmd)
	agentsCmd.AddCommand(agentsPrefClearCmd)
}

func runAgentsList(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}
	registry.SetPreferences(cfg.Agents.Preferences)

	var agentList []*agents.Agent

	// Filter by source if flags are set
	if agentsBuiltin {
		agentList, err = registry.ListBySource(agents.SourceBuiltin)
	} else if agentsLocal {
		agentList, err = registry.ListBySource(agents.SourceLocal)
	} else if agentsUser {
		agentList, err = registry.ListBySource(agents.SourceUser)
	} else {
		agentList, err = registry.List()
	}

	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	if len(agentList) == 0 {
		if agentsBuiltin || agentsLocal || agentsUser {
			fmt.Println("No agents found matching filter.")
		} else {
			fmt.Println("No agents configured.")
			fmt.Println()
			fmt.Println("Create one with: term-llm agents new <name>")
			fmt.Println("Or use a built-in: term-llm ask --agent reviewer ...")
		}
		return nil
	}

	// Group by source for display
	fmt.Printf("Available agents (%d):\n\n", len(agentList))

	// Track which sources we've seen
	var lastSource agents.AgentSource = -1

	for _, agent := range agentList {
		// Print source header if changed
		if agent.Source != lastSource {
			if lastSource != -1 {
				fmt.Println()
			}
			switch agent.Source {
			case agents.SourceLocal:
				localDir, _ := agents.GetLocalAgentsDir()
				fmt.Printf("  [local] %s/\n", localDir)
			case agents.SourceUser:
				userDir, _ := agents.GetUserAgentsDir()
				fmt.Printf("  [user] %s/\n", userDir)
			case agents.SourceBuiltin:
				fmt.Println("  [builtin]")
			}
			lastSource = agent.Source
		}

		// Print agent info
		fmt.Printf("    %s", agent.Name)
		if agent.Description != "" {
			fmt.Printf(" - %s", agent.Description)
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Println("Use with: term-llm ask --agent <name> ... or term-llm chat --agent <name>")
	return nil
}

func runAgentsNew(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate name
	if strings.ContainsAny(name, "/\\:*?\"<>|") {
		return fmt.Errorf("invalid agent name: %s", name)
	}

	// Determine base directory
	var baseDir string
	var err error
	if agentsLocal {
		baseDir, err = agents.GetLocalAgentsDir()
	} else {
		baseDir, err = agents.GetUserAgentsDir()
	}
	if err != nil {
		return fmt.Errorf("get agents dir: %w", err)
	}

	// Check if agent already exists
	agentDir := filepath.Join(baseDir, name)
	if _, err := os.Stat(agentDir); err == nil {
		return fmt.Errorf("agent already exists: %s", agentDir)
	}

	// Create agent
	if err := agents.CreateAgentDir(baseDir, name); err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	fmt.Printf("Created agent: %s\n\n", agentDir)
	fmt.Println("Files created:")
	fmt.Println("  agent.yaml  - Agent configuration")
	fmt.Println("  system.md   - System prompt template")
	fmt.Println()
	fmt.Printf("Edit with: term-llm agents edit %s\n", name)
	fmt.Printf("Use with:  term-llm ask --agent %s ...\n", name)

	return nil
}

func runAgentsShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}
	registry.SetPreferences(cfg.Agents.Preferences)

	agent, err := registry.Get(name)
	if err != nil {
		return err
	}

	// Display agent info
	fmt.Printf("Agent: %s\n", agent.Name)
	fmt.Printf("Source: %s\n", agent.Source.SourceName())
	if agent.SourcePath != "" {
		fmt.Printf("Path: %s\n", agent.SourcePath)
	}
	fmt.Println()

	if agent.Description != "" {
		fmt.Printf("Description: %s\n\n", agent.Description)
	}

	// Model settings
	if agent.Provider != "" || agent.Model != "" {
		fmt.Println("Model:")
		if agent.Provider != "" {
			fmt.Printf("  provider: %s\n", agent.Provider)
		}
		if agent.Model != "" {
			fmt.Printf("  model: %s\n", agent.Model)
		}
		fmt.Println()
	}

	// Tool settings
	if agent.HasEnabledList() {
		fmt.Printf("Tools (enabled): %s\n", strings.Join(agent.Tools.Enabled, ", "))
	} else if agent.HasDisabledList() {
		fmt.Printf("Tools (disabled): %s\n", strings.Join(agent.Tools.Disabled, ", "))
	}

	if len(agent.Shell.Allow) > 0 {
		fmt.Printf("Shell allow: %s\n", strings.Join(agent.Shell.Allow, ", "))
	}
	if agent.Shell.AutoRun {
		fmt.Println("Shell auto-run: true")
	}
	if len(agent.Shell.Scripts) > 0 {
		fmt.Println("Shell scripts:")
		// Sort script names for consistent output
		names := make([]string, 0, len(agent.Shell.Scripts))
		for name := range agent.Shell.Scripts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			script := agent.Shell.Scripts[name]
			// Truncate long scripts
			display := script
			if len(display) > 60 {
				display = display[:57] + "..."
			}
			fmt.Printf("  %s: %s\n", name, display)
		}
	}
	if len(agent.Read.Dirs) > 0 {
		fmt.Printf("Read dirs: %s\n", strings.Join(agent.Read.Dirs, ", "))
	}

	if agent.MaxTurns > 0 {
		fmt.Printf("Max turns: %d\n", agent.MaxTurns)
	}

	// MCP servers
	if len(agent.MCP) > 0 {
		fmt.Println()
		fmt.Println("MCP servers:")
		for _, m := range agent.MCP {
			if m.Command != "" {
				fmt.Printf("  - %s: %s\n", m.Name, m.Command)
			} else {
				fmt.Printf("  - %s\n", m.Name)
			}
		}
	}

	// System prompt
	if agent.SystemPrompt != "" {
		fmt.Println()
		fmt.Println("System prompt:")
		fmt.Println("---")
		// Show first 500 chars with ... if truncated
		prompt := agent.SystemPrompt
		if len(prompt) > 500 {
			prompt = prompt[:500] + "\n..."
		}
		fmt.Println(prompt)
		fmt.Println("---")
	}

	return nil
}

func runAgentsEdit(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}
	registry.SetPreferences(cfg.Agents.Preferences)

	agent, err := registry.Get(name)
	if err != nil {
		return err
	}

	// Built-in agents can't be edited directly
	if agent.Source == agents.SourceBuiltin {
		return fmt.Errorf("cannot edit built-in agent '%s'. Copy it first: term-llm agents copy %s my-%s", name, name, name)
	}

	// Get editor
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	// Open agent.yaml
	agentPath := filepath.Join(agent.SourcePath, "agent.yaml")
	editCmd := exec.Command(editor, agentPath)
	editCmd.Stdin = os.Stdin
	editCmd.Stdout = os.Stdout
	editCmd.Stderr = os.Stderr

	return editCmd.Run()
}

func runAgentsCopy(cmd *cobra.Command, args []string) error {
	srcName := args[0]
	destName := args[1]

	// Validate dest name
	if strings.ContainsAny(destName, "/\\:*?\"<>|") {
		return fmt.Errorf("invalid agent name: %s", destName)
	}

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}
	registry.SetPreferences(cfg.Agents.Preferences)

	srcAgent, err := registry.Get(srcName)
	if err != nil {
		return err
	}

	// Determine destination directory
	var destDir string
	if agentsLocal {
		destDir, err = agents.GetLocalAgentsDir()
	} else {
		destDir, err = agents.GetUserAgentsDir()
	}
	if err != nil {
		return fmt.Errorf("get agents dir: %w", err)
	}

	// Check if dest already exists
	destAgentDir := filepath.Join(destDir, destName)
	if _, err := os.Stat(destAgentDir); err == nil {
		return fmt.Errorf("agent already exists: %s", destAgentDir)
	}

	// Copy the agent
	if err := agents.CopyAgent(srcAgent, destDir, destName); err != nil {
		return fmt.Errorf("copy agent: %w", err)
	}

	fmt.Printf("Copied '%s' to '%s'\n", srcName, destAgentDir)
	fmt.Println()
	fmt.Printf("Edit with: term-llm agents edit %s\n", destName)
	fmt.Printf("Use with:  term-llm ask --agent %s ...\n", destName)

	return nil
}

func runAgentsPath(cmd *cobra.Command, args []string) error {
	localDir, _ := agents.GetLocalAgentsDir()
	userDir, _ := agents.GetUserAgentsDir()

	fmt.Println("Agent directories (searched in order):")
	fmt.Println()

	// Check if local dir exists
	if _, err := os.Stat(localDir); err == nil {
		fmt.Printf("  local: %s\n", localDir)
	} else {
		fmt.Printf("  local: %s (not created)\n", localDir)
	}

	// Check if user dir exists
	if _, err := os.Stat(userDir); err == nil {
		fmt.Printf("  user:  %s\n", userDir)
	} else {
		fmt.Printf("  user:  %s (not created)\n", userDir)
	}

	fmt.Println("  builtin: (embedded in binary)")

	return nil
}

// agentNameCompletion provides shell completion for agent names.
func agentNameCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	registry.SetPreferences(cfg.Agents.Preferences)

	agentList, err := registry.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var names []string
	for _, agent := range agentList {
		if strings.HasPrefix(agent.Name, toComplete) {
			names = append(names, agent.Name)
		}
	}

	return names, cobra.ShellCompDirectiveNoFileComp
}

// AgentFlagCompletion provides shell completion for the --agent flag.
func AgentFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return agentNameCompletion(cmd, nil, toComplete)
}

// agentPrefSetCompletion provides completion for "agents set <agent> key=value"
func agentPrefSetCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// First arg: agent name
	if len(args) == 0 {
		return agentNameCompletion(cmd, args, toComplete)
	}

	// Subsequent args: preference keys
	prefKeys := []string{
		"provider=",
		"model=",
		"max_turns=",
		"search=",
		"tools_enabled=",
		"tools_disabled=",
		"shell_allow=",
		"shell_auto_run=",
		"spawn_max_parallel=",
		"spawn_max_depth=",
		"spawn_timeout=",
		"spawn_allowed_agents=",
	}

	var completions []string
	for _, key := range prefKeys {
		if strings.HasPrefix(key, toComplete) {
			completions = append(completions, key)
		}
	}

	return completions, cobra.ShellCompDirectiveNoSpace
}

func runAgentsPrefSet(cmd *cobra.Command, args []string) error {
	agentName := args[0]
	keyValues := args[1:]

	// Validate that the agent exists (warn if not)
	cfg, err := loadConfigWithSetup()
	if err == nil {
		registry, err := agents.NewRegistry(agents.RegistryConfig{
			UseBuiltin:  cfg.Agents.UseBuiltin,
			SearchPaths: cfg.Agents.SearchPaths,
		})
		if err == nil {
			if _, err := registry.Get(agentName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: agent '%s' does not exist\n", agentName)
			}
		}
	}

	for _, kv := range keyValues {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid key=value pair: %s", kv)
		}
		key, value := parts[0], parts[1]

		keysSet, err := config.SetAgentPreference(agentName, key, value)
		if err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}

		// Report what was set (may be multiple for provider:model format)
		if len(keysSet) > 1 {
			// provider:model format was used
			provider, model := config.ParseProviderModel(value)
			fmt.Printf("Set %s.provider = %s\n", agentName, provider)
			fmt.Printf("Set %s.model = %s\n", agentName, model)
		} else {
			fmt.Printf("Set %s.%s = %s\n", agentName, key, value)
		}
	}

	return nil
}

func runAgentsPrefGet(cmd *cobra.Command, args []string) error {
	agentName := args[0]

	pref, found := config.GetAgentPreference(agentName)
	if !found {
		fmt.Printf("No preferences set for agent '%s'\n", agentName)
		return nil
	}

	fmt.Printf("Preferences for '%s':\n", agentName)

	// Model preferences
	if pref.Provider != "" {
		fmt.Printf("  provider: %s\n", pref.Provider)
	}
	if pref.Model != "" {
		fmt.Printf("  model: %s\n", pref.Model)
	}

	// Tool configuration
	if len(pref.ToolsEnabled) > 0 {
		fmt.Printf("  tools_enabled: %s\n", strings.Join(pref.ToolsEnabled, ", "))
	}
	if len(pref.ToolsDisabled) > 0 {
		fmt.Printf("  tools_disabled: %s\n", strings.Join(pref.ToolsDisabled, ", "))
	}

	// Shell settings
	if len(pref.ShellAllow) > 0 {
		fmt.Printf("  shell_allow: %s\n", strings.Join(pref.ShellAllow, ", "))
	}
	if pref.ShellAutoRun != nil {
		fmt.Printf("  shell_auto_run: %v\n", *pref.ShellAutoRun)
	}

	// Spawn settings
	if pref.SpawnMaxParallel != nil {
		fmt.Printf("  spawn_max_parallel: %d\n", *pref.SpawnMaxParallel)
	}
	if pref.SpawnMaxDepth != nil {
		fmt.Printf("  spawn_max_depth: %d\n", *pref.SpawnMaxDepth)
	}
	if pref.SpawnTimeout != nil {
		fmt.Printf("  spawn_timeout: %d\n", *pref.SpawnTimeout)
	}
	if len(pref.SpawnAllowedAgents) > 0 {
		fmt.Printf("  spawn_allowed_agents: %s\n", strings.Join(pref.SpawnAllowedAgents, ", "))
	}

	// Behavior
	if pref.MaxTurns != nil {
		fmt.Printf("  max_turns: %d\n", *pref.MaxTurns)
	}
	if pref.Search != nil {
		fmt.Printf("  search: %v\n", *pref.Search)
	}

	return nil
}

func runAgentsPrefClear(cmd *cobra.Command, args []string) error {
	agentName := args[0]

	if err := config.ClearAgentPreferences(agentName); err != nil {
		return fmt.Errorf("clear preferences: %w", err)
	}

	fmt.Printf("Cleared preferences for '%s'\n", agentName)
	return nil
}
