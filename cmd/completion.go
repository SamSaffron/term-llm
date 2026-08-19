package cmd

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

// Keep completion refreshes short: stale cached or curated models remain
// available when the local CLI or its remote catalog is slow.
const (
	grokBinCompletionRefreshTimeout = 5 * time.Second
	ollamaCompletionRefreshTimeout  = 2 * time.Second
)

var (
	refreshGrokBinModelsForCompletion = llm.RefreshGrokBinModelsIfStale
	refreshOllamaModelsForCompletion  = llm.RefreshOllamaModelsIfStale
)

// ProviderFlagCompletion handles --provider flag completion for LLM commands
func ProviderFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Try to load config for custom provider completions; nil is OK if it fails
	cfg, _ := config.Load()
	refreshGrokBinCompletionCache(toComplete, cfg)
	refreshOllamaCompletionCache(toComplete, cfg)
	completions := llm.GetProviderCompletions(toComplete, false, cfg)

	// A bare provider may still be extended with ":model", so suppress the
	// trailing space. An effort profile is already a complete flag value.
	if !strings.Contains(toComplete, ":") {
		return completions, providerFlagCompletionDirective(cfg, toComplete)
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

func refreshGrokBinCompletionCache(toComplete string, cfg *config.Config) {
	provider, _, completingModel := strings.Cut(toComplete, ":")
	if !completingModel {
		return
	}

	var providerCfg config.ProviderConfig
	if cfg != nil {
		providerCfg = cfg.Providers[provider]
		if len(providerCfg.Models) > 0 {
			return
		}
	}
	if config.InferProviderType(provider, providerCfg.Type) != config.ProviderTypeGrokBin {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), grokBinCompletionRefreshTimeout)
	defer cancel()
	_ = refreshGrokBinModelsForCompletion(ctx, providerCfg.Model, providerCfg.Env)
}

func refreshOllamaCompletionCache(toComplete string, cfg *config.Config) {
	provider, _, completingModel := strings.Cut(toComplete, ":")
	if !completingModel {
		provider = ""
		if cfg != nil {
			for name, providerCfg := range cfg.Providers {
				if strings.HasPrefix(toComplete, name+"-") && len(name) > len(provider) && config.InferProviderType(name, providerCfg.Type) == config.ProviderTypeOllama {
					provider = name
				}
			}
		}
		if provider == "" {
			return
		}
	}

	var providerCfg config.ProviderConfig
	if cfg != nil {
		providerCfg = cfg.Providers[provider]
		if len(providerCfg.Models) > 0 {
			return
		}
	}
	if config.InferProviderType(provider, providerCfg.Type) != config.ProviderTypeOllama {
		return
	}
	if err := providerCfg.ResolveForInference(); err != nil {
		return
	}
	if cfg != nil {
		cfg.Providers[provider] = providerCfg
	}

	ctx, cancel := context.WithTimeout(context.Background(), ollamaCompletionRefreshTimeout)
	defer cancel()
	_ = refreshOllamaModelsForCompletion(ctx, providerCfg.BaseURL, providerCfg.Model)
}

func ollamaModelCompletions(provider string, cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	providerCfg, ok := cfg.Providers[provider]
	if !ok || config.InferProviderType(provider, providerCfg.Type) != config.ProviderTypeOllama {
		return nil
	}
	refreshOllamaCompletionCache(provider+":", cfg)
	prefixed := llm.GetProviderCompletions(provider+":", false, cfg)
	models := make([]string, 0, len(prefixed))
	prefix := provider + ":"
	for _, completion := range prefixed {
		if model, ok := strings.CutPrefix(completion, prefix); ok {
			models = append(models, model)
		}
	}
	return models
}

func providerFlagCompletionDirective(cfg *config.Config, toComplete string) cobra.ShellCompDirective {
	directive := cobra.ShellCompDirectiveNoFileComp
	if !llm.IsConfiguredProviderEffortPrefix(cfg, toComplete) {
		directive |= cobra.ShellCompDirectiveNoSpace
	}
	return directive
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
// Used by commands like "mcp info <server>", "mcp run <server>" and "mcp remove <server>".
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

// MCPRunArgCompletion provides completions for "mcp run <server> <tool> [key=val] ...".
// Completes server names for the first arg, tool names and key= params for subsequent args.
// Tool/param data comes only from the local cache populated by "mcp info" and "mcp run".
// Shell completion must stay fast and must not cold-start external MCP servers.
func MCPRunArgCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	noFile := cobra.ShellCompDirectiveNoFileComp

	// First arg: server name
	if len(args) == 0 {
		return MCPServerArgCompletion(cmd, args, toComplete)
	}

	serverName := args[0]
	tools := mcp.LoadCachedTools(serverName)
	if tools == nil {
		return nil, noFile
	}

	// Figure out the "current tool" by scanning args after the server name.
	// Bare words (no = or {) are tool names; everything else is a param.
	var currentTool string
	for _, a := range args[1:] {
		if !strings.Contains(a, "=") && !strings.HasPrefix(a, "{") {
			currentTool = a
		}
	}

	// If toComplete contains "=", nothing to complete (user is typing a value)
	if strings.Contains(toComplete, "=") {
		return nil, noFile | cobra.ShellCompDirectiveNoSpace
	}

	// Build tool name list and a map for quick lookup
	toolNames := make([]string, 0, len(tools))
	toolMap := make(map[string]mcp.ToolSpec, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
		toolMap[t.Name] = t
	}

	var completions []string

	// If we have a current tool, offer its parameter keys as "key=" completions
	if t, ok := toolMap[currentTool]; ok {
		if props, ok := t.Schema["properties"].(map[string]any); ok {
			for key := range props {
				candidate := key + "="
				if strings.HasPrefix(candidate, toComplete) {
					completions = append(completions, candidate)
				}
			}
		}
	}

	// Also offer tool names (for starting a new tool call)
	for _, name := range toolNames {
		if strings.HasPrefix(name, toComplete) {
			completions = append(completions, name)
		}
	}

	sort.Strings(completions)
	return completions, noFile | cobra.ShellCompDirectiveNoSpace
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
// When typing "read_file,wr<TAB>", completes to "read_file,write_file".
func ToolsFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Get all available tool names (spec names)
	allTools := tools.ValidToolNames()
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

	// Show "all" as first option only when starting fresh (no tools entered yet)
	if len(alreadyEntered) == 0 && strings.HasPrefix("all", currentPrefix) {
		completions = append(completions, "all")
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

// UsageProviderFlagCompletion handles --provider flag completion for the usage command.
// Returns the specific providers that the usage command supports.
func UsageProviderFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Include aliases accepted by the usage command.
	providers := []string{"chatgpt", "claude-bin", "cursor-bin", "opencode-go", "claude-code", "claude", "copilot", "term-llm", "all"}
	var completions []string
	for _, p := range providers {
		if strings.HasPrefix(p, toComplete) {
			completions = append(completions, p)
		}
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

// CopilotScopeFlagCompletion handles --copilot-scope completion for the usage command.
func CopilotScopeFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	scopes := []string{"user", "org", "enterprise"}
	var completions []string
	for _, scope := range scopes {
		if strings.HasPrefix(scope, toComplete) {
			completions = append(completions, scope)
		}
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

// ExtractAgentFromArgs checks args for @agent-name syntax and returns the agent name
// and filtered args. Returns empty string if no @agent found.
func ExtractAgentFromArgs(args []string) (agentName string, filteredArgs []string) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "@") && len(arg) > 1 {
			agentName = arg[1:] // strip the @
		} else {
			filteredArgs = append(filteredArgs, arg)
		}
	}
	return agentName, filteredArgs
}

// AtAgentCompletion provides completions for @agent syntax in positional args.
// When typing "@<TAB>", completes to "@agent-builder", "@commit", etc.
func AtAgentCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Only complete if starts with @
	if !strings.HasPrefix(toComplete, "@") {
		return nil, cobra.ShellCompDirectiveDefault
	}

	// Get agent names
	agentNames, directive := AgentFlagCompletion(cmd, args, toComplete[1:])
	if directive == cobra.ShellCompDirectiveError {
		return nil, directive
	}

	// Prefix with @
	var completions []string
	for _, name := range agentNames {
		completions = append(completions, "@"+name)
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}
