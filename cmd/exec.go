package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	execPrintOnly      bool
	execSearch         bool
	execNoSearch       bool
	execDebug          bool
	execAutoPick       bool
	execMaxOpts        int
	execProvider       string
	execFiles          []string
	execMCP            string
	execMaxTurns       int
	execNativeSearch   bool
	execNoNativeSearch bool
	execNoWebFetch     bool
	// Tool flags
	execTools         string
	execReadDirs      []string
	execWriteDirs     []string
	execShellAllow    []string
	execSystemMessage string
	// Yolo/auto modes
	execYolo         bool
	execAutoApproval bool
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
	AddCommonFlags(execCmd,
		CommonCoreFlags|CommonSearchFlags|CommonMaxTurns|CommonFiles|CommonSkills,
		CommonFlagBindings{
			Provider:         &execProvider,
			Debug:            &execDebug,
			Search:           &execSearch,
			NoSearch:         &execNoSearch,
			NativeSearch:     &execNativeSearch,
			NoNativeSearch:   &execNoNativeSearch,
			NoWebFetch:       &execNoWebFetch,
			MCP:              &execMCP,
			MaxTurns:         &execMaxTurns,
			MaxTurnsDefault:  50,
			Tools:            &execTools,
			ReadDirs:         &execReadDirs,
			WriteDirs:        &execWriteDirs,
			ShellAllow:       &execShellAllow,
			SystemMessage:    &execSystemMessage,
			Files:            &execFiles,
			FilesDescription: "File(s) to include as context (supports globs, 'clipboard')",
			Yolo:             &execYolo,
			Auto:             &execAutoApproval,
			Skills:           &execSkills,
		})

	// Exec-specific flags
	execCmd.Flags().BoolVar(&execPrintOnly, "print-only", false, "Print command instead of executing")
	execCmd.Flags().BoolVarP(&execAutoPick, "auto-pick", "a", false, "Auto-execute the best suggestion without prompting")
	execCmd.Flags().IntVarP(&execMaxOpts, "max", "n", 0, "Maximum number of options to show (0 = no limit)")

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

	if execNoSearch {
		execSearch = false
	}

	initThemeFromConfig(cfg)

	// Detect shell
	shell := detectShell()

	// Read files if provided
	var files []input.FileContent
	if len(execFiles) > 0 {
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
	statsModel := ""
	defer func() {
		if showStats {
			stats.Finalize()
			setEstimatedStatsCost(stats, statsModel)
			fmt.Fprintln(cmd.ErrOrStderr(), stats.Render())
		}
	}()

	// Main loop for refinement
	instructions := cfg.Exec.Instructions
	if execSystemMessage != "" {
		instructions = execSystemMessage
	}
	forceTool := llm.SuggestCommandsToolName
	lastTurnForceTool := ""
	if strings.TrimSpace(execTools) != "" {
		forceTool = ""
		lastTurnForceTool = llm.SuggestCommandsToolName
	}

	runner := newCmdRunner(cfg, cmdRunnerOptions{
		Provider:           execProvider,
		ConfigSet:          true,
		ConfigProvider:     cfg.Exec.Provider,
		ConfigModel:        cfg.Exec.Model,
		ConfigInstructions: cfg.Exec.Instructions,
		Tools:              execTools,
		ReadDirs:           append([]string(nil), execReadDirs...),
		WriteDirs:          append([]string(nil), execWriteDirs...),
		ShellAllow:         append([]string(nil), execShellAllow...),
		MCP:                execMCP,
		MaxTurns:           execMaxTurns,
		Search:             execSearch,
		NoSearch:           execNoSearch,
		NativeSearch:       execNativeSearch,
		NoNativeSearch:     execNoNativeSearch,
		Yolo:               execYolo,
		Auto:               execAutoApproval,
		Debug:              execDebug,
		DebugRaw:           debugRaw,
		ErrWriter:          cmd.ErrOrStderr(),
	}).(*cmdRunner)
	execEnv, err := runner.prepare(ctx, runpkg.Request{
		Platform:                runpkg.PlatformExec,
		DeferSession:            true,
		Provider:                execProvider,
		Tools:                   execTools,
		ReadDirs:                append([]string(nil), execReadDirs...),
		WriteDirs:               append([]string(nil), execWriteDirs...),
		ShellAllow:              append([]string(nil), execShellAllow...),
		MCP:                     execMCP,
		MaxTurns:                execMaxTurns,
		Search:                  execBoolPtr(execSearch),
		NoSearch:                execNoSearch,
		Yolo:                    execYolo,
		Auto:                    execAutoApproval,
		Debug:                   debugMode,
		DebugRaw:                debugRaw,
		ForceExternalSearch:     execBoolPtr(resolveForceExternalSearch(cfg, execNativeSearch, execNoNativeSearch)),
		DisableExternalWebFetch: execNoWebFetch,
		ExtraTools:              []llm.ToolSpec{llm.SuggestCommandsToolSpec(numSuggestions)},
		ForceToolName:           forceTool,
		LastTurnForceToolName:   lastTurnForceTool,
	}, nil)
	if err != nil {
		return err
	}
	defer execEnv.Close()
	statsModel = execEnv.llmReq.Model

	for {
		systemPrompt := prompt.SuggestSystemPrompt(shell, instructions, numSuggestions, execSearch)
		userPrompt := prompt.SuggestUserPrompt(userInput, files, stdinContent)

		// Create progress channel for spinner updates
		progressCh := make(chan ui.ProgressUpdate, 10)

		// Set up approval hooks when tools are enabled to pause spinner during prompts
		var approvalHooks ui.ApprovalHookSetup
		if strings.TrimSpace(execTools) != "" {
			approvalHooks = func(pause, resume func()) {
				tools.SetApprovalHooks(pause, resume)
				tools.SetAskUserHooks(pause, resume)
			}
		}

		sink := &execRunSink{progressCh: progressCh, stats: stats, debug: debugMode || debugRaw, debugRaw: debugRaw}
		result, err := ui.RunWithSpinnerProgressAndHooks(ctx, debugMode || debugRaw, progressCh, func(ctx context.Context) (any, error) {
			defer close(progressCh)
			configureInteractiveSink(execEnv.runtime.toolMgr, sink)
			llmReq := execEnv.llmReq
			stats.SetModel(statsModel)
			stats.RequestStart()
			_, runErr := execEnv.runtime.RunWithEvents(ctx, false, false, []llm.Message{llm.SystemText(systemPrompt), llm.UserText(userPrompt)}, llmReq, func(ev llm.Event) error {
				sink.Event(ev)
				return nil
			})
			if runErr != nil {
				return nil, runErr
			}
			if sink.err != nil {
				return nil, sink.err
			}
			suggestions := sink.Suggestions()
			if len(suggestions) == 0 {
				return nil, fmt.Errorf("no suggestions returned")
			}
			return execSuggestionsResult{suggestions: suggestions, engine: execEnv.engine}, nil
		}, approvalHooks)
		tools.ClearApprovalHooks() // Safe to call even if hooks weren't set
		tools.ClearAskUserHooks()  // Safe to call even if hooks weren't set
		if err != nil {
			if err.Error() == "cancelled" {
				return nil
			}
			return fmt.Errorf("failed to get suggestions: %w", err)
		}

		suggestionResult, ok := result.(execSuggestionsResult)
		if !ok {
			return fmt.Errorf("unexpected suggestions result")
		}
		suggestions := suggestionResult.suggestions

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
		selected, refinement, err := ui.SelectCommand(suggestions, shell, suggestionResult.engine, allowNonTTY)
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

type execSuggestionsResult struct {
	suggestions []llm.CommandSuggestion
	engine      *llm.Engine
}

type execRunSink struct {
	progressCh chan<- ui.ProgressUpdate
	stats      *ui.SessionStats
	debug      bool
	debugRaw   bool

	suggestions []llm.CommandSuggestion
	err         error
	sentFirst   bool

	attemptInput, attemptOutput, attemptCached, attemptCacheWrite int
	attemptUsageCalls                                             int
}

func execBoolPtr(v bool) *bool { return &v }

func (s *execRunSink) PromptApproval(target string, isWrite, isShell bool, workDir string) (tools.ApprovalResult, error) {
	if isShell {
		return tools.RunShellApprovalUI(target, workDir)
	}
	return tools.RunFileApprovalUI(target, isWrite)
}

func (s *execRunSink) Event(event llm.Event) {
	if s == nil {
		return
	}
	// Handle tool execution events
	if event.Type == llm.EventToolExecStart {
		if event.ToolName != "" {
			if s.stats != nil {
				s.stats.ToolStart()
			}
			phase := ui.FormatToolPhase(event.ToolName, event.ToolInfo).Active
			if s.debug {
				fmt.Fprintf(os.Stderr, "  > %s\n", phase)
			}
		} else if s.stats != nil {
			s.stats.ToolEnd()
		}
		if event.ToolName == tools.AskUserToolName {
			return
		}
		phase := "Thinking"
		if event.ToolName != "" {
			phase = ui.FormatToolPhase(event.ToolName, event.ToolInfo).Active
		}
		s.sendProgress(ui.ProgressUpdate{Phase: phase})
		return
	}

	if event.Type == llm.EventToolExecEnd {
		if s.stats != nil {
			s.stats.ToolEnd()
		}
		s.resetAttemptUsage()
		if s.debug && event.ToolName != "" {
			phase := ui.FormatToolPhase(event.ToolName, event.ToolInfo)
			if event.ToolSuccess {
				fmt.Fprintf(os.Stderr, "  %s %s\n", ui.SuccessCircle(), phase.Completed)
			} else {
				fmt.Fprintf(os.Stderr, "  %s %s\n", ui.ErrorCircle(), phase.Completed)
			}
		}
	}

	if event.Type == llm.EventRetry {
		if s.stats != nil {
			s.stats.ScheduleRetryStart(event.RetryWaitSecs)
		}
		status := ui.FormatRetryStatus("Rate limited", event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs, 0, "...")
		s.sendProgress(ui.ProgressUpdate{Status: status})
		return
	}

	if event.Type == llm.EventAttemptDiscard {
		if s.stats != nil {
			s.stats.DiscardUsage(s.attemptInput, s.attemptOutput, s.attemptCached, s.attemptCacheWrite, s.attemptUsageCalls)
		}
		s.resetAttemptUsage()
		s.suggestions = nil
		return
	}

	if event.Type == llm.EventModelSwitch {
		model := event.Model
		if model == "" {
			model = event.Text
		}
		if s.stats != nil {
			s.stats.SetModel(model)
		}
		return
	}

	if event.Type == llm.EventUsage && event.Use != nil {
		if s.stats != nil {
			s.stats.GenerationEnd()
			s.stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
		}
		if !event.Use.BillableCountersZero() {
			s.attemptInput += event.Use.InputTokens
			s.attemptOutput += event.Use.OutputTokens
			s.attemptCached += event.Use.CachedInputTokens
			s.attemptCacheWrite += event.Use.CacheWriteTokens
			s.attemptUsageCalls++
		}
		return
	}

	if s.stats != nil && (event.Type == llm.EventTextDelta && event.Text != "" || event.Type == llm.EventReasoningDelta && (event.Text != "" || llm.IsEncryptedReasoningDelta(event))) {
		s.stats.ObserveOutput()
	}

	if !s.sentFirst {
		s.sentFirst = true
		s.sendProgress(ui.ProgressUpdate{Phase: "Responding"})
	}

	if event.Type == llm.EventError && event.Err != nil {
		s.err = event.Err
		return
	}
	if event.Type == llm.EventToolCall {
		// Tool calls are durable boundaries; a later retry must not discard the
		// provider request that produced the tool.
		s.resetAttemptUsage()
	}
	if event.Type == llm.EventToolCall && event.Tool != nil && event.Tool.Name == llm.SuggestCommandsToolName {
		llm.DebugToolCall(s.debug, *event.Tool)
		parsed, err := llm.ParseCommandSuggestions(*event.Tool)
		if err != nil {
			s.err = err
			return
		}
		s.suggestions = append(s.suggestions, parsed...)
	}
}

func (s *execRunSink) resetAttemptUsage() {
	s.attemptInput, s.attemptOutput = 0, 0
	s.attemptCached, s.attemptCacheWrite = 0, 0
	s.attemptUsageCalls = 0
}

func (s *execRunSink) sendProgress(update ui.ProgressUpdate) {
	if s == nil || s.progressCh == nil {
		return
	}
	select {
	case s.progressCh <- update:
	default:
	}
}

func (s *execRunSink) Suggestions() []llm.CommandSuggestion {
	if s == nil || len(s.suggestions) == 0 {
		return nil
	}
	return append([]llm.CommandSuggestion(nil), s.suggestions...)
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
