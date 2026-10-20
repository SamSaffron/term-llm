package cmd

import (
	"fmt"

	"sort"
	"strings"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"

	"github.com/spf13/cobra"
)

type serveMCPOptions = appserve.MCPOptions

var serveMCPFlags serveMCPOptions

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

	serveMCPCmd.Flags().StringVar(&serveMCPFlags.Tools, "tools", "", "Tools to expose (comma-separated, or 'all') [required]")
	serveMCPCmd.Flags().StringVar(&serveMCPFlags.EditFormat, "edit-format", "edit_file", "Edit tool flavor: edit_file (find/replace) or diff (unified diff)")
	serveMCPCmd.Flags().StringVar(&serveMCPFlags.Host, "host", "127.0.0.1", "Bind address (use 0.0.0.0 for remote access)")
	serveMCPCmd.Flags().IntVar(&serveMCPFlags.Port, "port", 8080, "Bind port")
	serveMCPCmd.Flags().StringVar(&serveMCPFlags.Token, "token", "", "Bearer token for auth (auto-generated if omitted)")
	serveMCPCmd.Flags().StringArrayVar(&serveMCPFlags.ReadDirs, "read-dir", nil, "Allowed read directories (repeatable)")
	serveMCPCmd.Flags().StringArrayVar(&serveMCPFlags.WriteDirs, "write-dir", nil, "Allowed write directories (repeatable)")
	serveMCPCmd.Flags().StringArrayVar(&serveMCPFlags.ShellAllow, "shell-allow", nil, "Allowed shell patterns (repeatable, glob syntax)")
	AddApprovalFlag(serveMCPCmd, &serveMCPFlags.Approval)
	serveMCPCmd.Flags().BoolVar(&serveMCPFlags.Yolo, "yolo", false, "Auto-approve all tool actions")
	serveMCPCmd.Flags().BoolVar(&serveMCPFlags.Auto, "auto", false, "Use guardian LLM review for unmatched shell and file approvals (requires configured LLM provider)")
	serveMCPCmd.MarkFlagsMutuallyExclusive("approval", "yolo", "auto")
	serveMCPCmd.Flags().BoolVarP(&serveMCPFlags.Debug, "debug", "d", false, "Verbose logging")

	_ = serveMCPCmd.RegisterFlagCompletionFunc("tools", MCPToolsFlagCompletion)
}

func resolveServeMCPApprovalMode(cmd *cobra.Command, cfg *config.Config, opts *serveMCPOptions) (resolvedApprovalMode, error) {
	childExplicit := cmd != nil && (cmd.Flags().Changed("approval") || cmd.Flags().Changed("auto") || cmd.Flags().Changed("yolo"))
	// Pre-command normalization rewrites `term-llm --approval auto serve mcp`
	// to a flag on the parent serve command. Preserve that explicit choice for
	// the distinct serve_mcp surface while rejecting a second child-level mode.
	parentExplicit := serveCmd.Flags().Changed("approval") || serveCmd.Flags().Changed("auto") || serveCmd.Flags().Changed("yolo")
	if childExplicit && parentExplicit {
		return resolvedApprovalMode{}, fmt.Errorf("--approval, --auto, and --yolo are mutually exclusive across serve and serve mcp")
	}
	if parentExplicit {
		return resolveCommandApprovalMode(serveCmd, approvalSurfaceServeMCP, cfg, nil, serveFlags.Approval, serveFlags.Auto, serveFlags.Yolo)
	}
	return resolveCommandApprovalMode(cmd, approvalSurfaceServeMCP, cfg, nil, opts.Approval, opts.Auto, opts.Yolo)
}

func runServeMCP(cmd *cobra.Command, args []string) error {
	approvalOptions := serveMCPFlags
	invocation := appserve.MCPInvocation{
		Stdout: cmd.OutOrStdout(),
		Stderr: cmd.ErrOrStderr(),
		ResolveApproval: func(cfg *config.Config) (resolvedApprovalMode, error) {
			return resolveServeMCPApprovalMode(cmd, cfg, &approvalOptions)
		},
	}
	return appserve.RunMCP(cmd.Context(), &serveMCPFlags, args, invocation)
}

// MCPToolsFlagCompletion provides completions for the --tools flag on serve mcp.
func MCPToolsFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	allTools := appserve.MCPAllToolNames()
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
