package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/samsaffron/term-llm/internal/app/reload"
	"log"
	"sort"
	"strings"
	"time"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"

	"github.com/samsaffron/term-llm/internal/mcphttp"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/search"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
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

// MCPAllToolNames returns every tool name the MCP server can expose (for "all").
func MCPAllToolNames() []string {
	all := make([]string, 0, len(mcpAllLocalTools)+len(mcpAllWebTools))
	all = append(all, mcpAllLocalTools...)
	all = append(all, mcpAllWebTools...)
	return all
}

func runServeMCPWithOptions(ctx context.Context, invocation appserve.MCPInvocation, args []string, opts *serveMCPOptions) error {
	if opts.Tools == "" {
		return fmt.Errorf("--tools is required\n\nExamples:\n  term-llm serve mcp --tools all\n  term-llm serve mcp --tools read_file,grep,glob,shell")
	}
	if opts.Port <= 0 || opts.Port > 65535 {
		return fmt.Errorf("invalid --port %d (must be 1-65535)", opts.Port)
	}
	if opts.EditFormat != "edit_file" && opts.EditFormat != "diff" {
		return fmt.Errorf("invalid --edit-format %q (must be edit_file or diff)", opts.EditFormat)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if invocation.ResolveApproval == nil {
		return fmt.Errorf("serve mcp approval resolver is required")
	}
	resolvedApproval, err := invocation.ResolveApproval(cfg)
	if err != nil {
		return err
	}
	opts.Approval = resolvedApproval.Mode.String()
	opts.Yolo = resolvedApproval.Mode == tools.ModeYolo
	opts.Auto = resolvedApproval.Mode == tools.ModeAuto

	// Resolve requested tool names.
	requestedTools := parseMCPToolsFlag(opts.Tools)

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
	if opts.EditFormat == "diff" {
		localToolNames = swapEditTool(localToolNames, tools.EditFileToolName, tools.UnifiedDiffToolName)
	}

	// Build local tool registry.
	toolConfig := tools.ToolConfig{
		Enabled:    localToolNames,
		ReadDirs:   opts.ReadDirs,
		WriteDirs:  opts.WriteDirs,
		ShellAllow: opts.ShellAllow,
	}

	perms, err := toolConfig.BuildPermissions()
	if err != nil {
		return fmt.Errorf("build permissions: %w", err)
	}
	approvalMgr := tools.NewApprovalManager(perms)
	defer approvalMgr.Close()
	if err := applyResolvedApprovalMode(cfg, approvalMgr, resolvedApproval, approvalRuntimeOptions{Headless: true}); err != nil {
		return err
	}
	reportApprovalMode(invocation.Stderr, opts.Debug, resolvedApproval, approvalMgr)

	registry, err := tools.NewLocalToolRegistry(&toolConfig, cfg, approvalMgr)
	if err != nil {
		return fmt.Errorf("create tool registry: %w", err)
	}
	registry.SetServeMode(true, "")

	// Build web tools.
	var webSearchTool *llm.WebSearchTool
	if wantWebSearch {
		if searchErr := search.Available(cfg); searchErr != nil {
			log.Printf("warning: search provider not configured, skipping web_search: %v", searchErr)
			wantWebSearch = false
		} else {
			webSearchTool = llm.NewWebSearchTool(search.NewLazySearcher(cfg))
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

	// Collect MCP tool specs. Keep the executor registered for stale/in-flight
	// calls, but do not advertise redundant workspace grants in yolo mode.
	var mcpTools []mcphttp.ToolSpec
	localSpecs := tools.FilterToolSpecsForApprovalMode(registry.GetSpecs(), approvalMgr)
	for _, spec := range localSpecs {
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
	executor := func(ctx context.Context, name string, args json.RawMessage) (mcphttp.ToolResult, error) {
		// Own the executor independently of the SDK handler: disconnect/cancellation
		// may end the HTTP response while a tool is still finishing its effects.
		ctx, release, err := restart.Default.Enter(ctx)
		if err != nil {
			return mcphttp.ToolResult{}, err
		}
		defer release()
		// Check web tools first.
		if name == mcpWebSearchToolName && webSearchTool != nil {
			out, err := webSearchTool.Execute(ctx, args)
			if err != nil {
				return mcphttp.ToolResult{}, err
			}
			return serveMCPToolResult(out), nil
		}
		if name == mcpReadURLToolName && readURLTool != nil {
			out, err := readURLTool.Execute(ctx, args)
			if err != nil {
				return mcphttp.ToolResult{}, err
			}
			return serveMCPToolResult(out), nil
		}

		// Local tools.
		tool, ok := registry.Get(name)
		if !ok {
			return mcphttp.ToolResult{}, fmt.Errorf("unknown tool: %s", name)
		}
		out, err := tool.Execute(ctx, args)
		if err != nil {
			return mcphttp.ToolResult{}, err
		}
		return serveMCPToolResult(out), nil
	}

	// Resolve auth token.
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		token = reload.MCPToken()
	}
	if token == "" {
		generated, err := generateServeToken()
		if err != nil {
			return fmt.Errorf("generate auth token: %w", err)
		}
		token = generated
	}

	// Start MCP server.
	server := mcphttp.NewServer(executor)
	server.SetDebug(opts.Debug)
	server.HandlerMiddleware = restart.Default.Handler
	server.ConnState = restart.Default.HTTPConnections()

	runCtx, stop := signal.NotifyContextWithParent(ctx)
	defer stop()

	url, actualToken, err := server.StartOnAddress(opts.Host, opts.Port, token, mcpTools)
	if err != nil {
		return fmt.Errorf("start MCP server: %w", err)
	}

	// MCP HTTP is stateless. Keep listeners live until exec (Go sockets are
	// close-on-exec), so a failed exec leaves the original service usable.
	stopReload, err := reload.Bind(runCtx, "TERM_LLM_MCP_RELOAD_TOKEN="+actualToken)
	if err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(stopCtx)
		return err
	}
	defer stopReload()

	// Print server info.
	toolNames := make([]string, len(mcpTools))
	for i, t := range mcpTools {
		toolNames[i] = t.Name
	}
	sort.Strings(toolNames)

	fmt.Fprintf(invocation.Stderr, "MCP server listening on %s\n", url)
	fmt.Fprintf(invocation.Stderr, "auth token: %s\n", actualToken)
	fmt.Fprintf(invocation.Stderr, "tools: %s\n", strings.Join(toolNames, ", "))
	if opts.Yolo {
		fmt.Fprintln(invocation.Stderr, "yolo: true (all operations auto-approved)")
	} else if opts.Auto {
		fmt.Fprintln(invocation.Stderr, "auto: true (unmatched shell and file operations require guardian approval; unavailable reviewer denies)")
	}

	<-runCtx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Stop(shutdownCtx)
}

// parseMCPToolsFlag parses the --tools value for the MCP server.
// "all" expands to all MCP-appropriate tools.
func parseMCPToolsFlag(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "all" || trimmed == "*" {
		return MCPAllToolNames()
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
		allowed := MCPAllToolNames()
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
