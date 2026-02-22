package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	execPrintOnly      bool
	execSearch         bool
	execDebug          bool
	execAutoPick       bool
	execMaxOpts        int
	execProvider       string
	execFiles          []string
	execMCP            string
	execMaxTurns       int
	execNativeSearch   bool
	execNoNativeSearch bool
	// Tool flags
	execTools         string
	execReadDirs      []string
	execWriteDirs     []string
	execShellAllow    []string
	execSystemMessage string
	// Yolo mode
	execYolo bool
	// Skills flag
	execSkills string
)

const (
	allowAutoRunEnv = "TERM_LLM_ALLOW_AUTORUN"
	allowNonTTYEnv  = "TERM_LLM_ALLOW_NON_TTY"
)

var execCmd = &cobra.Command{
	Use:   "exec <request>",
	Short: "Translate natural language to CLI commands",
	Long: `Get command suggestions from AI and execute one.

By default, shows an interactive selection UI. Use --auto-pick to
automatically execute the highest-likelihood suggestion.

Safety: auto-pick execution requires TERM_LLM_ALLOW_AUTORUN=1, and
non-TTY selection requires TERM_LLM_ALLOW_NON_TTY=1 (unless --print-only).

Examples:
  term-llm exec "list files by size"
  term-llm exec "find go files" --auto-pick    # auto-execute best
  term-llm exec "install latest node" -s       # with web search
  term-llm exec "compress folder" -n 5         # show max 5 options
  term-llm exec "disk usage" -p                # print only, don't execute
  term-llm exec -f log.txt "find errors"       # with file context
  term-llm exec -f clipboard "explain this"    # from clipboard
  git diff | term-llm exec "commit message"    # from stdin`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExec,
}

func init() {
	// Common flags shared across commands
	AddProviderFlag(execCmd, &execProvider)
	AddDebugFlag(execCmd, &execDebug)
	AddSearchFlag(execCmd, &execSearch)
	AddNativeSearchFlags(execCmd, &execNativeSearch, &execNoNativeSearch)
	AddMCPFlag(execCmd, &execMCP)
	AddMaxTurnsFlag(execCmd, &execMaxTurns, 20)
	AddToolFlags(execCmd, &execTools, &execReadDirs, &execWriteDirs, &execShellAllow)
	AddSystemMessageFlag(execCmd, &execSystemMessage)
	AddFileFlag(execCmd, &execFiles, "File(s) to include as context (supports globs, 'clipboard')")

	// Exec-specific flags
	execCmd.Flags().BoolVar(&execPrintOnly, "print-only", false, "Print command instead of executing")
	execCmd.Flags().BoolVarP(&execAutoPick, "auto-pick", "a", false, "Auto-execute the best suggestion without prompting")
	execCmd.Flags().IntVarP(&execMaxOpts, "max", "n", 0, "Maximum number of options to show (0 = no limit)")
	AddYoloFlag(execCmd, &execYolo)
	AddSkillsFlag(execCmd, &execSkills)

	// Additional completions
	if err := execCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register tools completion: %v", err))
	}
	rootCmd.AddCommand(execCmd)
}

func runExec(cmd *cobra.Command, args []string) error {
	userInput := strings.Join(args, " ")
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	if err := applyProviderOverrides(cfg, cfg.Exec.Provider, cfg.Exec.Model, execProvider); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	caps := provider.Capabilities()
	engine := newEngine(provider, cfg)

	// Set up debug logger if enabled
	debugLogger, err := createDebugLogger(cfg)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
	}
	if debugLogger != nil {
		engine.SetDebugLogger(debugLogger)
		defer debugLogger.Close()
	}

	// Initialize local tools if --tools flag is set
	var localToolSpecs []llm.ToolSpec
	if execTools != "" {
		toolConfig := buildToolConfig(execTools, execReadDirs, execWriteDirs, execShellAllow, cfg)
		if errs := toolConfig.Validate(); len(errs) > 0 {
			return fmt.Errorf("invalid tool config: %v", errs[0])
		}
		toolMgr, err := tools.NewToolManager(&toolConfig, cfg)
		if err != nil {
			return fmt.Errorf("failed to initialize tools: %w", err)
		}
		wireImageRecorder(toolMgr.Registry, "", "")
		// Enable yolo mode if flag is set
		if execYolo {
			toolMgr.ApprovalMgr.SetYoloMode(true)
		}
		toolMgr.ApprovalMgr.PromptFunc = tools.HuhApprovalPrompt
		toolMgr.SetupEngine(engine)
		localToolSpecs = toolMgr.GetSpecs()

		// Wire spawn_agent runner if enabled
		if err := WireSpawnAgentRunner(cfg, toolMgr, execYolo); err != nil {
			return err
		}
	}

	// Initialize MCP servers if --mcp flag is set
	var mcpManager *mcp.Manager
	if execMCP != "" {
		mcpOpts := &MCPOptions{
			Provider: provider,
			YoloMode: execYolo,
		}
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			mcpOpts.Model = providerCfg.Model
		}
		mcpManager, err = enableMCPServersWithFeedback(ctx, execMCP, engine, cmd.ErrOrStderr(), mcpOpts)
		if err != nil {
			return err
		}
		if mcpManager != nil {
			defer mcpManager.StopAll()
		}
	}

	// Detect shell
	shell := detectShell()

	// Read files if provided
	var files []input.FileContent
	if len(execFiles) > 0 {
		var err error
		files, err = input.ReadFiles(execFiles)
		if err != nil {
			return fmt.Errorf("failed to read files: %w", err)
		}
	}

	// Read stdin if available
	stdinContent, err := input.ReadStdin()
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	// Determine number of suggestions: use -n flag if set, otherwise config default
	numSuggestions := cfg.Exec.Suggestions
	if execMaxOpts > 0 {
		numSuggestions = execMaxOpts
	}

	debugMode := execDebug

	// Track session stats
	stats := ui.NewSessionStats()
	defer func() {
		if showStats {
			stats.Finalize()
			fmt.Fprintln(cmd.ErrOrStderr(), stats.Render())
		}
	}()

	// Main loop for refinement
	instructions := cfg.Exec.Instructions
	if execSystemMessage != "" {
		instructions = execSystemMessage
	}
	for {
		systemPrompt := prompt.SuggestSystemPrompt(shell, instructions, numSuggestions, execSearch)
		userPrompt := prompt.SuggestUserPrompt(userInput, files, stdinContent)
		// Build tools list: suggest_commands + any local tools
		reqTools := []llm.ToolSpec{llm.SuggestCommandsToolSpec(numSuggestions)}
		reqTools = append(reqTools, localToolSpecs...)

		// Use auto tool choice if we have local tools (so model can use them first),
		// otherwise force suggest_commands. Always force suggest_commands on last turn.
		// If provider doesn't support tool_choice, rely on the strong prompt hint instead.
		var toolChoice llm.ToolChoice
		var lastTurnToolChoice *llm.ToolChoice
		if caps.SupportsToolChoice {
			toolChoice = llm.ToolChoice{Mode: llm.ToolChoiceName, Name: llm.SuggestCommandsToolName}
			if len(localToolSpecs) > 0 {
				toolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
				lastTurnToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceName, Name: llm.SuggestCommandsToolName}
			}
		}

		req := llm.Request{
			Messages: []llm.Message{
				llm.SystemText(systemPrompt),
				llm.UserText(userPrompt),
			},
			Tools:               reqTools,
			ToolChoice:          toolChoice,
			LastTurnToolChoice:  lastTurnToolChoice,
			ParallelToolCalls:   true,
			Search:              execSearch,
			ForceExternalSearch: resolveForceExternalSearch(cfg, execNativeSearch, execNoNativeSearch),
			MaxTurns:            execMaxTurns,
			Debug:               debugMode,
			DebugRaw:            debugRaw,
		}

		// Create progress channel for spinner updates
		progressCh := make(chan ui.ProgressUpdate, 10)

		// Set up approval hooks when tools are enabled to pause spinner during prompts
		var approvalHooks ui.ApprovalHookSetup
		if len(localToolSpecs) > 0 {
			approvalHooks = func(pause, resume func()) {
				tools.SetApprovalHooks(pause, resume)
				tools.SetAskUserHooks(pause, resume)
			}
		}

		result, err := ui.RunWithSpinnerProgressAndHooks(ctx, debugMode || debugRaw, progressCh, func(ctx context.Context) (any, error) {
			defer close(progressCh)
			return collectSuggestions(ctx, engine, req, progressCh, stats)
		}, approvalHooks)
		tools.ClearApprovalHooks() // Safe to call even if hooks weren't set
		tools.ClearAskUserHooks()  // Safe to call even if hooks weren't set
		if err != nil {
			if err.Error() == "cancelled" {
				return nil
			}
			return fmt.Errorf("failed to get suggestions: %w", err)
		}

		suggestions, ok := result.([]llm.CommandSuggestion)
		if !ok {
			return fmt.Errorf("unexpected suggestions result")
		}

		// Sort by likelihood (highest first)
		sort.Slice(suggestions, func(i, j int) bool {
			return suggestions[i].Likelihood > suggestions[j].Likelihood
		})

		// Limit options if --max is set
		if execMaxOpts > 0 && len(suggestions) > execMaxOpts {
			suggestions = suggestions[:execMaxOpts]
		}

		// Auto-pick mode: execute the best suggestion immediately
		if execAutoPick {
			command := suggestions[0].Command
			if !execPrintOnly && !envEnabled(allowAutoRunEnv) {
				return fmt.Errorf("auto-pick requires %s=1 to execute; use --print-only or set %s=1", allowAutoRunEnv, allowAutoRunEnv)
			}
			if execPrintOnly {
				fmt.Println(command)
				return nil
			}
			return executeCommand(command, shell)
		}

		// Interactive mode: show selection UI (with help support via 'i' key)
		allowNonTTY := execPrintOnly || envEnabled(allowNonTTYEnv)
		selected, refinement, err := ui.SelectCommand(suggestions, shell, engine, allowNonTTY)
		if err != nil {
			if err.Error() == "cancelled" {
				return nil
			}
			return fmt.Errorf("selection cancelled: %w", err)
		}

		// Handle "something else" option - refinement is already collected inline
		if selected == ui.SomethingElse {
			if refinement == "" {
				continue
			}
			// Append refinement to original request
			userInput = fmt.Sprintf("%s (%s)", userInput, refinement)
			continue
		}

		// Print-only mode: just output the command for shell integration
		if execPrintOnly {
			if selected != "" {
				fmt.Println(selected)
			}
			return nil
		}

		// Execute the selected command
		return executeCommand(selected, shell)
	}
}

func collectSuggestions(ctx context.Context, engine *llm.Engine, req llm.Request, progressCh chan<- ui.ProgressUpdate, stats *ui.SessionStats) ([]llm.CommandSuggestion, error) {
	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	var suggestions []llm.CommandSuggestion
	sentFirstToken := false
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		// Handle tool execution events
		if event.Type == llm.EventToolExecStart {
			if event.ToolName != "" {
				stats.ToolStart()
				// In debug mode, spinner is disabled so output to stderr directly
				phase := ui.FormatToolPhase(event.ToolName, event.ToolInfo).Active
				if req.Debug || req.DebugRaw {
					fmt.Fprintf(os.Stderr, "  > %s\n", phase)
				}
			} else {
				stats.ToolEnd()
			}
			// Skip phase update for ask_user - it has its own UI
			if event.ToolName == tools.AskUserToolName {
				continue
			}
			var phase string
			if event.ToolName == "" {
				// Empty tool name means back to thinking
				phase = "Thinking"
			} else {
				phase = ui.FormatToolPhase(event.ToolName, event.ToolInfo).Active
			}
			select {
			case progressCh <- ui.ProgressUpdate{Phase: phase}:
			default:
			}
			continue
		}

		// Handle tool execution end events in debug mode
		if event.Type == llm.EventToolExecEnd && (req.Debug || req.DebugRaw) {
			if event.ToolName != "" {
				phase := ui.FormatToolPhase(event.ToolName, event.ToolInfo)
				if event.ToolSuccess {
					fmt.Fprintf(os.Stderr, "  %s %s\n", ui.SuccessCircle(), phase.Completed)
				} else {
					fmt.Fprintf(os.Stderr, "  %s %s\n", ui.ErrorCircle(), phase.Completed)
				}
			}
		}

		// Handle retry events (rate limit backoff)
		if event.Type == llm.EventRetry {
			status := fmt.Sprintf("Rate limited (%d/%d), waiting %.0fs...",
				event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs)
			select {
			case progressCh <- ui.ProgressUpdate{Status: status}:
			default:
			}
			continue
		}

		// Track usage for stats
		if event.Type == llm.EventUsage && event.Use != nil {
			stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
			continue
		}

		// Send phase update on first event
		if !sentFirstToken {
			sentFirstToken = true
			select {
			case progressCh <- ui.ProgressUpdate{Phase: "Responding"}:
			default:
			}
		}

		if event.Type == llm.EventError && event.Err != nil {
			return nil, event.Err
		}
		if event.Type == llm.EventToolCall && event.Tool != nil && event.Tool.Name == llm.SuggestCommandsToolName {
			llm.DebugToolCall(req.Debug, *event.Tool)
			parsed, err := llm.ParseCommandSuggestions(*event.Tool)
			if err != nil {
				return nil, err
			}
			suggestions = append(suggestions, parsed...)
		}
	}

	if len(suggestions) == 0 {
		return nil, fmt.Errorf("no suggestions returned")
	}
	return suggestions, nil
}

func envEnabled(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch value {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}
