package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcphttp"
	"github.com/samsaffron/term-llm/internal/search"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

// MCP tool names for web tools (not part of local tool registry).
const (
	mcpWebSearchToolName = "web_search"
	mcpReadURLToolName   = "read_url"
)

// mcpAllLocalTools lists the local tools exposed via MCP (order matters for display).
var mcpAllLocalTools = []string{
	tools.ReadFileToolName,
	tools.WriteFileToolName,
	tools.EditFileToolName,
	tools.ShellToolName,
	tools.GrepToolName,
	tools.GlobToolName,
	tools.ImageGenerateToolName,
}

// mcpAllWebTools lists the web tools exposed via MCP.
var mcpAllWebTools = []string{
	mcpWebSearchToolName,
	mcpReadURLToolName,
}

// mcpAllowedTools is the set of tool names valid for serve mcp.
// unified_diff is allowed only via --edit-format diff (swapped in for edit_file).
var mcpAllowedTools map[string]bool

func init() {
	mcpAllowedTools = make(map[string]bool)
	for _, t := range mcpAllLocalTools {
		mcpAllowedTools[t] = true
	}
	for _, t := range mcpAllWebTools {
		mcpAllowedTools[t] = true
	}
	// unified_diff is valid only when injected by --edit-format diff
	mcpAllowedTools[tools.UnifiedDiffToolName] = true
}

// mcpAllToolNames returns every tool name the MCP server can expose (for "all").
func mcpAllToolNames() []string {
	all := make([]string, 0, len(mcpAllLocalTools)+len(mcpAllWebTools))
	all = append(all, mcpAllLocalTools...)
	all = append(all, mcpAllWebTools...)
	return all
}

var (
	serveMCPHost       string
	serveMCPPort       int
	serveMCPToken      string
	serveMCPTools      string
	serveMCPEditFormat string
	serveMCPReadDirs   []string
	serveMCPWriteDirs  []string
	serveMCPShellAllow []string
	serveMCPYolo       bool
	serveMCPDebug      bool
)

var serveMCPCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run an MCP server exposing term-llm tools over HTTP",
	Long: `Start an MCP (Model Context Protocol) server that exposes term-llm's
tools over HTTP. Any MCP client can connect and use the tools.

The --tools flag is required and specifies which tools to expose.

Examples:
  term-llm serve mcp --tools all
  term-llm serve mcp --tools read_file,grep,glob,shell
  term-llm serve mcp --tools all --host 0.0.0.0 --port 9090
  term-llm serve mcp --tools all --yolo

Tools available:
  read_file, write_file, edit_file, shell, grep, glob   (file & search)
  web_search, read_url                                   (web)
  image_generate                                         (image)
  all                                                    (everything applicable)`,
	RunE: runServeMCP,
}

func init() {
	serveCmd.AddCommand(serveMCPCmd)

	serveMCPCmd.Flags().StringVar(&serveMCPTools, "tools", "", "Tools to expose (comma-separated, or 'all') [required]")
	serveMCPCmd.Flags().StringVar(&serveMCPEditFormat, "edit-format", "edit_file", "Edit tool flavor: edit_file (find/replace) or diff (unified diff)")
	serveMCPCmd.Flags().StringVar(&serveMCPHost, "host", "127.0.0.1", "Bind address (use 0.0.0.0 for remote access)")
	serveMCPCmd.Flags().IntVar(&serveMCPPort, "port", 8080, "Bind port")
	serveMCPCmd.Flags().StringVar(&serveMCPToken, "token", "", "Bearer token for auth (auto-generated if omitted)")
	serveMCPCmd.Flags().StringArrayVar(&serveMCPReadDirs, "read-dir", nil, "Allowed read directories (repeatable)")
	serveMCPCmd.Flags().StringArrayVar(&serveMCPWriteDirs, "write-dir", nil, "Allowed write directories (repeatable)")
	serveMCPCmd.Flags().StringArrayVar(&serveMCPShellAllow, "shell-allow", nil, "Allowed shell patterns (repeatable, glob syntax)")
	serveMCPCmd.Flags().BoolVar(&serveMCPYolo, "yolo", false, "Auto-approve all tool actions")
	serveMCPCmd.Flags().BoolVarP(&serveMCPDebug, "debug", "d", false, "Verbose logging")

	_ = serveMCPCmd.RegisterFlagCompletionFunc("tools", MCPToolsFlagCompletion)
}

func runServeMCP(cmd *cobra.Command, args []string) error {
	if serveMCPTools == "" {
		return fmt.Errorf("--tools is required\n\nExamples:\n  term-llm serve mcp --tools all\n  term-llm serve mcp --tools read_file,grep,glob,shell")
	}
	if serveMCPPort <= 0 || serveMCPPort > 65535 {
		return fmt.Errorf("invalid --port %d (must be 1-65535)", serveMCPPort)
	}
	if serveMCPEditFormat != "edit_file" && serveMCPEditFormat != "diff" {
		return fmt.Errorf("invalid --edit-format %q (must be edit_file or diff)", serveMCPEditFormat)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve requested tool names.
	requestedTools := parseMCPToolsFlag(serveMCPTools)

	// Validate every requested name against the curated allowlist.
	if err := validateMCPToolNames(requestedTools); err != nil {
		return err
	}

	// Separate local tools from web tools.
	var localToolNames []string
	var wantWebSearch, wantReadURL bool
	for _, name := range requestedTools {
		switch name {
		case mcpWebSearchToolName:
			wantWebSearch = true
		case mcpReadURLToolName:
			wantReadURL = true
		default:
			localToolNames = append(localToolNames, name)
		}
	}

	// Apply edit format: swap edit_file for unified_diff if --edit-format=diff.
	if serveMCPEditFormat == "diff" {
		localToolNames = swapEditTool(localToolNames, tools.EditFileToolName, tools.UnifiedDiffToolName)
	}

	// Build local tool registry.
	toolConfig := tools.ToolConfig{
		Enabled:    localToolNames,
		ReadDirs:   serveMCPReadDirs,
		WriteDirs:  serveMCPWriteDirs,
		ShellAllow: serveMCPShellAllow,
	}

	perms, err := toolConfig.BuildPermissions()
	if err != nil {
		return fmt.Errorf("build permissions: %w", err)
	}
	approvalMgr := tools.NewApprovalManager(perms)
	if serveMCPYolo {
		approvalMgr.SetYoloMode(true)
	}

	registry, err := tools.NewLocalToolRegistry(&toolConfig, cfg, approvalMgr)
	if err != nil {
		return fmt.Errorf("create tool registry: %w", err)
	}
	registry.SetServeMode(true, "")

	// Build web tools.
	var webSearchTool *llm.WebSearchTool
	if wantWebSearch {
		searcher, searchErr := search.NewSearcher(cfg)
		if searchErr != nil {
			log.Printf("warning: search provider not configured, skipping web_search: %v", searchErr)
			wantWebSearch = false
		} else {
			webSearchTool = llm.NewWebSearchTool(searcher)
		}
	}

	var readURLTool *llm.ReadURLTool
	if wantReadURL {
		readURLTool = newReadURLToolForConfig(cfg)
		if readURLTool == nil {
			log.Printf("warning: skipping read_url because search.fetch_provider is none")
			wantReadURL = false
		}
	}

	// Collect MCP tool specs.
	var mcpTools []mcphttp.ToolSpec
	for _, spec := range registry.GetSpecs() {
		mcpTools = append(mcpTools, mcphttp.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Schema:      spec.Schema,
		})
	}
	if wantWebSearch && webSearchTool != nil {
		spec := webSearchTool.Spec()
		mcpTools = append(mcpTools, mcphttp.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Schema:      spec.Schema,
		})
	}
	if wantReadURL && readURLTool != nil {
		spec := readURLTool.Spec()
		mcpTools = append(mcpTools, mcphttp.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Schema:      spec.Schema,
		})
	}

	if len(mcpTools) == 0 {
		return fmt.Errorf("no tools to expose (all requested tools were skipped)")
	}

	// Build executor that routes to the right tool.
	executor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		// Check web tools first.
		if name == mcpWebSearchToolName && webSearchTool != nil {
			out, err := webSearchTool.Execute(ctx, args)
			if err != nil {
				return "", err
			}
			return out.Content, nil
		}
		if name == mcpReadURLToolName && readURLTool != nil {
			out, err := readURLTool.Execute(ctx, args)
			if err != nil {
				return "", err
			}
			return out.Content, nil
		}

		// Local tools.
		tool, ok := registry.Get(name)
		if !ok {
			return "", fmt.Errorf("unknown tool: %s", name)
		}
		out, err := tool.Execute(ctx, args)
		if err != nil {
			return "", err
		}
		return out.Content, nil
	}

	// Resolve auth token.
	token := strings.TrimSpace(serveMCPToken)
	if token == "" {
		generated, err := generateServeToken()
		if err != nil {
			return fmt.Errorf("generate auth token: %w", err)
		}
		token = generated
	}

	// Start MCP server.
	server := mcphttp.NewServer(executor)
	server.SetDebug(serveMCPDebug)

	ctx, stop := signal.NotifyContext()
	defer stop()

	url, actualToken, err := server.StartOnAddress(serveMCPHost, serveMCPPort, token, mcpTools)
	if err != nil {
		return fmt.Errorf("start MCP server: %w", err)
	}

	// Print server info.
	toolNames := make([]string, len(mcpTools))
	for i, t := range mcpTools {
		toolNames[i] = t.Name
	}
	sort.Strings(toolNames)

	fmt.Fprintf(os.Stderr, "MCP server listening on %s\n", url)
	fmt.Fprintf(os.Stderr, "auth token: %s\n", actualToken)
	fmt.Fprintf(os.Stderr, "tools: %s\n", strings.Join(toolNames, ", "))
	if serveMCPYolo {
		fmt.Fprintf(os.Stderr, "yolo: true (all operations auto-approved)\n")
	}

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Stop(shutdownCtx)
}

// parseMCPToolsFlag parses the --tools value for the MCP server.
// "all" expands to all MCP-appropriate tools.
func parseMCPToolsFlag(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "all" || trimmed == "*" {
		return mcpAllToolNames()
	}
	parts := strings.Split(value, ",")
	var result []string
	seen := make(map[string]bool)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return result
}

// validateMCPToolNames rejects any tool name not in the curated MCP allowlist.
func validateMCPToolNames(names []string) error {
	var invalid []string
	for _, name := range names {
		if !mcpAllowedTools[name] {
			invalid = append(invalid, name)
		}
	}
	if len(invalid) > 0 {
		allowed := mcpAllToolNames()
		sort.Strings(allowed)
		return fmt.Errorf("unknown tool(s) for serve mcp: %s\nallowed: %s",
			strings.Join(invalid, ", "), strings.Join(allowed, ", "))
	}
	return nil
}

// swapEditTool replaces oldTool with newTool in the list.
func swapEditTool(names []string, oldTool, newTool string) []string {
	result := make([]string, 0, len(names))
	for _, n := range names {
		if n == oldTool {
			result = append(result, newTool)
		} else {
			result = append(result, n)
		}
	}
	return result
}

// MCPToolsFlagCompletion provides completions for the --tools flag on serve mcp.
func MCPToolsFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	allTools := mcpAllToolNames()
	sort.Strings(allTools)

	// Parse comma-separated list.
	var alreadyEntered []string
	var currentPrefix string
	if idx := strings.LastIndex(toComplete, ","); idx >= 0 {
		alreadyEntered = strings.Split(toComplete[:idx], ",")
		currentPrefix = toComplete[idx+1:]
	} else {
		currentPrefix = toComplete
	}

	enteredSet := make(map[string]bool)
	for _, t := range alreadyEntered {
		enteredSet[strings.TrimSpace(t)] = true
	}

	var completions []string
	prefix := strings.Join(alreadyEntered, ",")
	if prefix != "" {
		prefix += ","
	}

	// Show "all" only when starting fresh.
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
