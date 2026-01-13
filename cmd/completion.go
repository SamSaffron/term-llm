package cmd

import (
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

// ProviderFlagCompletion handles --provider flag completion for LLM commands
func ProviderFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Try to load config for custom provider completions; nil is OK if it fails
	cfg, _ := config.Load()
	completions := llm.GetProviderCompletions(toComplete, false, cfg)

	// If completing provider name (no colon), don't add space so user can type ":"
	if !strings.Contains(toComplete, ":") {
		return completions, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

// ImageProviderFlagCompletion handles --provider flag completion for image commands
func ImageProviderFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	completions := llm.GetProviderCompletions(toComplete, true, nil)

	// If completing provider name (no colon), don't add space so user can type ":"
	if !strings.Contains(toComplete, ":") {
		return completions, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

// MCPServerArgCompletion provides completions for MCP server names as positional arguments.
// Used by commands like "mcp test <server>" and "mcp remove <server>".
func MCPServerArgCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Only complete first argument
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	cfg, err := mcp.LoadConfig()
	if err != nil || cfg == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var completions []string
	for _, server := range cfg.ServerNames() {
		if strings.HasPrefix(server, toComplete) {
			completions = append(completions, server)
		}
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

// MCPFlagCompletion provides completions for --mcp flag with comma-separated support.
// When typing "playwright,file<TAB>", completes to "playwright,filesystem".
func MCPFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := mcp.LoadConfig()
	if err != nil || cfg == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	allServers := cfg.ServerNames()
	if len(allServers) == 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// Parse comma-separated list
	var alreadyEntered []string
	var currentPrefix string
	if idx := strings.LastIndex(toComplete, ","); idx >= 0 {
		alreadyEntered = strings.Split(toComplete[:idx], ",")
		currentPrefix = toComplete[idx+1:]
	} else {
		currentPrefix = toComplete
	}

	// Build set of already-entered servers
	enteredSet := make(map[string]bool)
	for _, s := range alreadyEntered {
		enteredSet[strings.TrimSpace(s)] = true
	}

	// Filter: exclude already-entered, match prefix
	var completions []string
	prefix := strings.Join(alreadyEntered, ",")
	if prefix != "" {
		prefix += ","
	}
	for _, server := range allServers {
		if enteredSet[server] {
			continue
		}
		if strings.HasPrefix(server, currentPrefix) {
			completions = append(completions, prefix+server)
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}

// ToolsFlagCompletion provides completions for --tools flag with comma-separated support.
// When typing "read,wr<TAB>", completes to "read,write".
func ToolsFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Get all available tool names
	allTools := make([]string, 0, len(tools.ToolNameMapping))
	for name := range tools.ToolNameMapping {
		allTools = append(allTools, name)
	}
	sort.Strings(allTools)

	// Parse comma-separated list
	var alreadyEntered []string
	var currentPrefix string
	if idx := strings.LastIndex(toComplete, ","); idx >= 0 {
		alreadyEntered = strings.Split(toComplete[:idx], ",")
		currentPrefix = toComplete[idx+1:]
	} else {
		currentPrefix = toComplete
	}

	// Build set of already-entered tools
	enteredSet := make(map[string]bool)
	for _, t := range alreadyEntered {
		enteredSet[strings.TrimSpace(t)] = true
	}

	// Filter: exclude already-entered, match prefix
	var completions []string
	prefix := strings.Join(alreadyEntered, ",")
	if prefix != "" {
		prefix += ","
	}
	for _, tool := range allTools {
		if enteredSet[tool] {
			continue
		}
		if strings.HasPrefix(tool, currentPrefix) {
			completions = append(completions, prefix+tool)
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}
