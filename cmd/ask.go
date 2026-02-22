package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/input"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	askDebug          bool
	askSearch         bool
	askText           bool
	askProvider       string
	askFiles          []string
	askMCP            string
	askMaxTurns       int
	askNativeSearch   bool
	askNoNativeSearch bool
	// Tool flags
	askTools         string
	askReadDirs      []string
	askWriteDirs     []string
	askShellAllow    []string
	askSystemMessage string
	// Agent flag
	askAgent string
	// Yolo mode
	askYolo bool
	// Skills flag
	askSkills string
	// Session resume flag
	askResume string
)

var askCmd = &cobra.Command{
	Use:   "ask [@agent] <question>",
	Short: "Ask a question and stream the answer",
	Long: `Ask the LLM a question and receive a streaming response.

Examples:
  term-llm ask "What is the capital of France?"
  term-llm ask "How do I reverse a string in Go?"
  term-llm ask "What is the latest version of Node.js?" -s
  term-llm ask "Explain the difference between TCP and UDP" -d
  term-llm ask "List 5 programming languages" --text
  term-llm ask -f code.go "Explain this code"
  term-llm ask -f code.go:10-50 "Explain this function"
  term-llm ask -f clipboard "What is this?"
  cat error.log | term-llm ask "What went wrong?"

Agent examples (use @agent shortcut or --agent flag):
  term-llm ask @reviewer "Review this code" -f main.go
  term-llm ask @commit-message              (uses default prompt)
  term-llm ask @commit-message "focus on the bug fix"
  term-llm ask @editor "Add error handling" -f utils.go
  term-llm ask --agent researcher "Find info about Go 1.22"

Line range syntax for files:
  main.go       - Include entire file
  main.go:11-22 - Include only lines 11-22
  main.go:11-   - Include lines 11 to end of file
  main.go:-22   - Include lines 1-22`,
	Args:              cobra.MinimumNArgs(0),
	RunE:              runAsk,
	ValidArgsFunction: AtAgentCompletion,
}

func init() {
	// Common flags shared across commands
	AddProviderFlag(askCmd, &askProvider)
	AddDebugFlag(askCmd, &askDebug)
	AddSearchFlag(askCmd, &askSearch)
	AddNativeSearchFlags(askCmd, &askNativeSearch, &askNoNativeSearch)
	AddMCPFlag(askCmd, &askMCP)
	AddMaxTurnsFlag(askCmd, &askMaxTurns, 20)
	AddToolFlags(askCmd, &askTools, &askReadDirs, &askWriteDirs, &askShellAllow)
	AddSystemMessageFlag(askCmd, &askSystemMessage)
	AddFileFlag(askCmd, &askFiles, "File(s) to include as context (supports globs, line ranges like file.go:10-20, 'clipboard')")
	AddAgentFlag(askCmd, &askAgent)

	// Ask-specific flags
	askCmd.Flags().BoolVarP(&askText, "text", "t", false, "Output plain text instead of rendered markdown")
	AddYoloFlag(askCmd, &askYolo)
	AddSkillsFlag(askCmd, &askSkills)

	// Session resume flag - NoOptDefVal allows --resume without a value
	askCmd.Flags().StringVarP(&askResume, "resume", "r", "", "Continue a session (empty for most recent, or session ID)")
	askCmd.Flags().Lookup("resume").NoOptDefVal = " " // space means "flag was passed without value"

	// Additional completions
	if err := askCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register agent completion: %v", err))
	}
	rootCmd.AddCommand(askCmd)
}

func runAsk(cmd *cobra.Command, args []string) error {
	// Extract @agent from args if present
	atAgent, filteredArgs := ExtractAgentFromArgs(args)
	if atAgent != "" && askAgent == "" {
		askAgent = atAgent
	}

	question := strings.Join(filteredArgs, " ")
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Load agent if specified
	agent, err := LoadAgent(askAgent, cfg)
	if err != nil {
		return err
	}

	// Handle default prompt for agents invoked without a message
	if question == "" {
		if agent == nil {
			return fmt.Errorf("question required (or use @agent with a default prompt)")
		}
		if agent.DefaultPrompt == "" {
			return fmt.Errorf("agent %q has no default prompt; provide a question", agent.Name)
		}
		question = agent.DefaultPrompt
	}

	// Apply provider overrides: CLI > agent > config
	agentProvider := ""
	agentModel := ""
	if agent != nil {
		agentProvider = agent.Provider
		agentModel = agent.Model
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, askProvider, agentProvider, agentModel); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
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

	// Initialize skills system
	skillsSetup := SetupSkills(&cfg.Skills, askSkills, cmd.ErrOrStderr())

	// Resolve all settings: CLI > agent > config
	settings, err := ResolveSettings(cfg, agent, CLIFlags{
		Provider:      askProvider,
		Tools:         askTools,
		ReadDirs:      askReadDirs,
		WriteDirs:     askWriteDirs,
		ShellAllow:    askShellAllow,
		MCP:           askMCP,
		SystemMessage: askSystemMessage,
		MaxTurns:      askMaxTurns,
		MaxTurnsSet:   cmd.Flags().Changed("max-turns"),
		Search:        askSearch,
		Files:         askFiles,
	}, cfg.Ask.Provider, cfg.Ask.Model, cfg.Ask.Instructions, cfg.Ask.MaxTurns, 20)
	if err != nil {
		return err
	}

	// Initialize session store and handle --resume BEFORE tool/MCP initialization
	// so that session settings can override settings.Tools, settings.MCP, etc.
	store, storeCleanup := InitSessionStore(cfg, cmd.ErrOrStderr())
	defer storeCleanup()
	var sess *session.Session
	var sessionMessages []llm.Message

	// Handle --resume flag - apply session settings before tool/MCP setup
	resuming := cmd.Flags().Changed("resume")
	if resuming {
		if store == nil {
			return fmt.Errorf("session storage is disabled; cannot resume")
		}
		resumeID := strings.TrimSpace(askResume)
		if resumeID == "" {
			sess, _ = store.GetCurrent(ctx)
			if sess == nil {
				summaries, _ := store.List(ctx, session.ListOptions{Limit: 1})
				if len(summaries) > 0 {
					sess, _ = store.Get(ctx, summaries[0].ID)
				}
			}
		} else {
			sess, _ = store.GetByPrefix(ctx, resumeID)
		}
		if sess == nil {
			return fmt.Errorf("no session to resume")
		}

		// Update current session marker so --resume without ID targets this session
		_ = store.SetCurrent(ctx, sess.ID)
		// Mark session as active since we're resuming it for a new turn
		_ = store.UpdateStatus(ctx, sess.ID, session.StatusActive)

		// Apply session settings for flags not explicitly set on CLI
		// (unconditionally - session may have had search/tools/MCP disabled)
		if !cmd.Flags().Changed("search") {
			settings.Search = sess.Search
		}
		if !cmd.Flags().Changed("tools") {
			settings.Tools = sess.Tools
		}
		if !cmd.Flags().Changed("mcp") {
			settings.MCP = sess.MCP
		}

		// Load session history
		sessionMsgs, _ := store.GetMessages(ctx, sess.ID, 0, 0)
		for _, msg := range sessionMsgs {
			sessionMessages = append(sessionMessages, msg.ToLLMMessage())
		}
	}

	// Resolve session ID early so tool wiring can capture it
	sessionID := ""
	if sess != nil {
		sessionID = sess.ID
	}
	if sessionID == "" && store != nil && !resuming {
		sessionID = session.NewID()
	}
	settings.SessionID = sessionID

	// Initialize local tools if we have any
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		return err
	}
	var outputTool *tools.SetOutputTool
	if toolMgr != nil {
		// Enable yolo mode if flag is set
		if askYolo {
			toolMgr.ApprovalMgr.SetYoloMode(true)
		}

		// Register output tool if agent configures one
		if agent != nil && agent.OutputTool.IsConfigured() {
			agentCfg := agent.OutputTool
			param := agentCfg.Param
			if param == "" {
				param = "content" // default
			}
			outputTool = toolMgr.Registry.RegisterOutputTool(agentCfg.Name, param, agentCfg.Description)
			// Re-register tools with engine after output tool was added
			toolMgr.SetupEngine(engine)
		}

		// PromptFunc is set in streamWithGlamour to use bubbletea UI

		// Wire spawn_agent runner if enabled (with session tracking)
		var parentSessionID string
		if sess != nil {
			parentSessionID = sess.ID
		}
		if err := WireSpawnAgentRunnerWithStore(cfg, toolMgr, askYolo, store, parentSessionID); err != nil {
			return err
		}

	}

	RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	// Initialize MCP servers if any (after session settings are applied)
	var mcpManager *mcp.Manager
	if settings.MCP != "" {
		mcpOpts := &MCPOptions{
			Provider: provider,
			YoloMode: askYolo,
		}
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			mcpOpts.Model = providerCfg.Model
		}
		mcpManager, err = enableMCPServersWithFeedback(ctx, settings.MCP, engine, cmd.ErrOrStderr(), mcpOpts)
		if err != nil {
			return err
		}
		if mcpManager != nil {
			defer mcpManager.StopAll()
		}
	}

	// Read files if provided
	var files []input.FileContent
	if len(askFiles) > 0 {
		files, err = input.ReadFiles(askFiles)
		if err != nil {
			return fmt.Errorf("failed to read files: %w", err)
		}
	}

	// Read stdin if available
	stdinContent, err := input.ReadStdin()
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	userPrompt := prompt.AskUserPrompt(question, files, stdinContent)

	// Create new session if not resuming
	if !resuming && store != nil {
		if sessionID == "" {
			sessionID = session.NewID()
		}
		modelName := "unknown"
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			modelName = providerCfg.Model
		}
		agentName := ""
		if agent != nil {
			agentName = agent.Name
		}
		sess = &session.Session{
			ID:        sessionID,
			Provider:  provider.Name(),
			Model:     modelName,
			Mode:      session.ModeAsk,
			Agent:     agentName,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Search:    settings.Search,
			Tools:     settings.Tools,
			MCP:       settings.MCP,
			Status:    session.StatusActive,
		}
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			sess.CWD = cwd
		}
		_ = store.Create(ctx, sess)
	}

	// Sequence numbers are now auto-allocated by the store (pass Sequence: -1)

	// Use system prompt from resolved settings (already expanded)
	instructions := settings.SystemPrompt

	instructions = InjectSkillsMetadata(instructions, skillsSetup)

	// Build messages in correct order: system -> history -> new user
	// Providers expect system message first
	var messages []llm.Message

	// Check if session history already starts with a system message
	historyHasSystem := len(sessionMessages) > 0 && sessionMessages[0].Role == llm.RoleSystem

	if instructions != "" && !historyHasSystem {
		// Add system message first (only if not already in history)
		messages = append(messages, llm.SystemText(instructions))
	}

	// Add session history (if resuming)
	messages = append(messages, sessionMessages...)

	// Add new user message
	messages = append(messages, llm.UserText(userPrompt))

	debugMode := askDebug
	if sess != nil {
		sessionID = sess.ID
	}
	req := llm.Request{
		SessionID:           sessionID,
		Messages:            messages,
		Search:              settings.Search,
		ForceExternalSearch: resolveForceExternalSearch(cfg, askNativeSearch, askNoNativeSearch),
		ParallelToolCalls:   true,
		MaxTurns:            settings.MaxTurns,
		Debug:               debugMode,
		DebugRaw:            debugRaw,
	}

	// Add tools to request if any are registered (local or MCP)
	if toolMgr != nil || mcpManager != nil {
		if specs := llm.ToolSpecsForRequest(engine.Tools(), settings.Search); len(specs) > 0 {
			req.Tools = specs
			req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
		}
	}

	// Check if we're in a TTY and can use glamour
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	useGlamour := !askText && isTTY && !debugRaw

	// Create stream adapter for unified event handling with proper buffering
	adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
	if sess != nil {
		adapter.Stats().SeedTotals(sess.InputTokens, sess.OutputTokens, sess.CachedInputTokens, sess.ToolCalls, sess.LLMTurns)
	}

	// For glamour mode, create the tea.Program and set PromptUIFunc BEFORE starting the stream
	// This avoids a race condition where tool execution starts before PromptUIFunc is set
	var teaProgram *tea.Program
	if useGlamour && toolMgr != nil {
		model := newAskStreamModel()
		model.store = store
		model.sess = sess

		// Set main provider/model for subagent comparison
		// Use cfg.DefaultProvider (e.g. "chatgpt") for cleaner display
		mainModelName := ""
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			mainModelName = providerCfg.Model
		}
		model.subagentTracker.SetMainProviderModel(cfg.DefaultProvider, mainModelName)

		teaProgram = tea.NewProgram(model, tea.WithoutSignalHandler())

		// Set up spawn_agent event callback for subagent progress visibility
		if spawnTool := toolMgr.GetSpawnAgentTool(); spawnTool != nil {
			spawnTool.SetEventCallback(func(callID string, event tools.SubagentEvent) {
				// Use goroutine to avoid blocking subagent execution if TUI message channel backs up
				go teaProgram.Send(askSubagentProgressMsg{CallID: callID, Event: event})
			})
		}

		// Set up the improved approval UI with git-aware heuristics
		toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
			// Flush content and suppress spinner before releasing terminal
			done := make(chan struct{})
			teaProgram.Send(askFlushBeforeApprovalMsg{Done: done})
			<-done

			// Pause the TUI
			teaProgram.ReleaseTerminal()
			defer func() {
				teaProgram.RestoreTerminal()
				teaProgram.Send(askResumeFromExternalUIMsg{})
			}()

			// Run the appropriate approval UI
			if isShell {
				return tools.RunShellApprovalUI(path)
			}
			return tools.RunFileApprovalUI(path, isWrite)
		}

		// Set up ask_user hooks to pause/resume the TUI during the interactive UI
		start, end := tools.CreateTUIHooks(teaProgram, func() {
			done := make(chan struct{})
			teaProgram.Send(askFlushBeforeAskUserMsg{Done: done})
			<-done // Wait for flush to complete
		})
		// Wrap end hook to also send resume message after terminal is restored
		originalEnd := end
		end = func() {
			originalEnd()
			teaProgram.Send(askResumeFromExternalUIMsg{})
		}
		tools.SetAskUserHooks(start, end)
	} else if toolMgr != nil {
		// Non-TUI mode: set up approval UI directly (no tea.Program to pause)
		toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
			if isShell {
				return tools.RunShellApprovalUI(path)
			}
			return tools.RunFileApprovalUI(path, isWrite)
		}
	}

	// Save user message BEFORE streaming (incremental save)
	// Capture start time for duration tracking in callback
	streamStartTime := time.Now()
	if store != nil && sess != nil {
		// Save system message first if this is a new session with instructions
		if instructions != "" && !historyHasSystem {
			sysMsg := &session.Message{
				SessionID:   sess.ID,
				Role:        llm.RoleSystem,
				Parts:       []llm.Part{{Type: llm.PartText, Text: instructions}},
				TextContent: instructions,
				CreatedAt:   time.Now(),
				Sequence:    -1, // Auto-allocate sequence
			}
			_ = store.AddMessage(ctx, sess.ID, sysMsg)
		}

		userMsg := &session.Message{
			SessionID:   sess.ID,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: userPrompt}},
			TextContent: userPrompt,
			CreatedAt:   time.Now(),
			Sequence:    -1, // Auto-allocate sequence
		}
		_ = store.AddMessage(ctx, sess.ID, userMsg)
		_ = store.IncrementUserTurns(ctx, sess.ID)
		sess.UserTurns++ // Keep in-memory value in sync

		// Update session summary from first user message
		if sess.Summary == "" {
			sess.Summary = session.TruncateSummary(question)
			_ = store.Update(ctx, sess)
		}

		// Set up response callback to save assistant message immediately (before tool execution)
		// This ensures the message is persisted even if tool execution fails/crashes
		engine.SetResponseCompletedCallback(func(ctx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
			// Calculate duration from stream start
			durationMs := time.Since(streamStartTime).Milliseconds()

			sessionMsg := session.NewMessage(sess.ID, assistantMsg, -1)
			sessionMsg.DurationMs = durationMs
			_ = store.AddMessage(ctx, sess.ID, sessionMsg)
			return nil
		})

		// Set up turn callback for tool result messages and metrics
		engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
			// Save messages (tool results, or assistant message when no tools were executed)
			for _, msg := range turnMessages {
				sessionMsg := session.NewMessage(sess.ID, msg, -1)
				// Set duration for assistant messages (when responseCallback didn't run)
				if msg.Role == llm.RoleAssistant {
					sessionMsg.DurationMs = time.Since(streamStartTime).Milliseconds()
				}
				_ = store.AddMessage(ctx, sess.ID, sessionMsg)
			}
			// Update metrics
			_ = store.UpdateMetrics(ctx, sess.ID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens)
			return nil
		})
	}

	// Enable context compaction or tracking for models with known context window data.
	engine.ConfigureContextManagement(provider, cfg.DefaultProvider, activeModel(cfg), cfg.AutoCompact)

	errChan := make(chan error, 1)
	go func() {
		stream, err := engine.Stream(ctx, req)
		if err != nil {
			errChan <- err
			return
		}
		defer stream.Close()
		// ProcessStream handles all events and closes the channel when done
		adapter.ProcessStream(ctx, stream)
		errChan <- nil
	}()

	// Set up text collection for output capture (commit_editmsg, on_complete, or session save)
	var collector *textCollector
	events := adapter.Events()
	needsCollector := (agent != nil && (agent.Output == "commit_editmsg" || agent.OnComplete != "")) || (store != nil && sess != nil)
	if needsCollector {
		collector = &textCollector{}
		events = collector.wrapEvents(events)
	}

	if useGlamour {
		err = streamWithGlamour(ctx, events, teaProgram, store, sess)
	} else {
		err = streamPlainText(ctx, events)
	}
	tools.ClearAskUserHooks() // Safe to call even if hooks weren't set

	if err != nil {
		return err
	}

	// Clear callbacks and update status
	engine.SetResponseCompletedCallback(nil)
	engine.SetTurnCompletedCallback(nil)

	streamErr := <-errChan
	if streamErr != nil {
		// Update session status based on error type
		if store != nil && sess != nil {
			if errors.Is(streamErr, context.Canceled) {
				_ = store.UpdateStatus(ctx, sess.ID, session.StatusInterrupted)
			} else {
				_ = store.UpdateStatus(ctx, sess.ID, session.StatusError)
			}
		}
		if errors.Is(streamErr, context.Canceled) {
			return nil
		}
		return fmt.Errorf("streaming failed: %w", streamErr)
	}

	// Update session status to complete
	if store != nil && sess != nil {
		_ = store.UpdateStatus(ctx, sess.ID, session.StatusComplete)
		_ = store.SetCurrent(ctx, sess.ID)
	}

	// Run on_complete handler if configured
	if agent != nil && agent.OnComplete != "" {
		var output string
		if outputTool != nil && outputTool.Value() != "" {
			output = outputTool.Value() // Tool output (preferred)
		} else if collector != nil {
			output = collector.Text() // Fallback to text
		}

		if output != "" {
			if err := runOnComplete(agent.OnComplete, output); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: on_complete failed: %v\n", err)
			}
		}
	} else if agent != nil && agent.Output == "commit_editmsg" {
		// Backwards compat: keep old output: commit_editmsg (deprecated path)
		if collector != nil {
			if err := writeCommitEditMsg(collector.Text()); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to write commit message: %v\n", err)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "\nCommit message written to .git/COMMIT_EDITMSG and .git/GITGUI_MSG\n")
				fmt.Fprintf(cmd.ErrOrStderr(), "Run 'git commit' to use it.\n")
			}
		}
	}

	if showStats {
		adapter.Stats().Finalize()
		fmt.Fprintln(cmd.ErrOrStderr(), adapter.Stats().Render())
	}

	return nil
}

// streamPlainText streams text directly without formatting
func streamPlainText(ctx context.Context, events <-chan ui.StreamEvent) error {
	// Track pending tools with their status
	type toolEntry struct {
		callID  string
		name    string
		info    string
		success bool
		done    bool
	}
	var pendingTools []toolEntry
	seenToolStarts := make(map[string]bool) // Track seen tool starts to prevent duplicates
	seenToolEnds := make(map[string]bool)   // Track seen tool ends to prevent duplicates
	printedAny := false
	trailingNewlines := 0
	afterToolBoundary := false
	newlineCompactor := ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)

	printTools := func() {
		if len(pendingTools) == 0 {
			return
		}
		if printedAny {
			fmt.Print(ui.NewlinePadding(trailingNewlines, ui.SectionBreakTrailingNewlines))
		}
		for _, t := range pendingTools {
			phase := ui.FormatToolPhase(t.name, t.info)
			if t.success {
				fmt.Printf("%s %s\n", ui.SuccessCircle(), phase.Completed)
			} else {
				fmt.Printf("%s %s\n", ui.ErrorCircle(), phase.Completed)
			}
		}
		pendingTools = nil
		printedAny = true
		trailingNewlines = ui.SectionBreakTrailingNewlines
		afterToolBoundary = true
	}

	ensureFinalSpacer := func() {
		if !printedAny {
			fmt.Println()
			return
		}
		fmt.Print(ui.NewlinePadding(trailingNewlines, ui.FinalSpacerTrailingNewlines))
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				if len(pendingTools) > 0 {
					printTools()
				}
				ensureFinalSpacer()
				return nil
			}

			switch ev.Type {
			case ui.StreamEventRetry:
				fmt.Fprintf(os.Stderr, "\rRate limited (%d/%d), waiting %.0fs...\n",
					ev.RetryAttempt, ev.RetryMax, ev.RetryWait)

			case ui.StreamEventUsage:
				// Skip usage events in plain text mode
				continue

			case ui.StreamEventPhase:
				// Print WARNING phases to stderr, skip others
				if strings.HasPrefix(ev.Phase, llm.WarningPhasePrefix) {
					fmt.Fprintf(os.Stderr, "\n%s\n", ev.Phase)
				}
				continue

			case ui.StreamEventToolEnd:
				// Skip duplicate tool end events (only dedupe when ToolCallID is not empty)
				if ev.ToolCallID != "" {
					if seenToolEnds[ev.ToolCallID] {
						continue
					}
					seenToolEnds[ev.ToolCallID] = true
				}
				// Find and update the tool entry by callID
				for i := range pendingTools {
					if pendingTools[i].callID == ev.ToolCallID && !pendingTools[i].done {
						pendingTools[i].success = ev.ToolSuccess
						pendingTools[i].done = true
						break
					}
				}
				// Check if all tools are done
				allDone := true
				for _, t := range pendingTools {
					if !t.done {
						allDone = false
						break
					}
				}
				if allDone && len(pendingTools) > 0 {
					printTools()
				}

			case ui.StreamEventToolStart:
				// Skip duplicate tool start events (only dedupe when ToolCallID is not empty)
				if ev.ToolCallID != "" {
					if seenToolStarts[ev.ToolCallID] {
						continue
					}
					seenToolStarts[ev.ToolCallID] = true
				}
				pendingTools = append(pendingTools, toolEntry{
					callID: ev.ToolCallID,
					name:   ev.ToolName,
					info:   ev.ToolInfo,
				})

			case ui.StreamEventText:
				text := ev.Text
				if afterToolBoundary && text != "" {
					// Keep exactly one blank line between tools and following text.
					fmt.Print(ui.NewlinePadding(trailingNewlines, ui.FinalSpacerTrailingNewlines))
					trailingNewlines = ui.FinalSpacerTrailingNewlines
					text = strings.TrimLeft(text, "\n")
					afterToolBoundary = false
				}
				text = newlineCompactor.CompactChunk(text)
				if text != "" {
					fmt.Print(text)
					printedAny = true
					trailingNewlines = ui.CountTrailingNewlines(text)
				}

			case ui.StreamEventImage:
				// Display image inline in plain text mode
				if ev.ImagePath != "" {
					if rendered := ui.RenderInlineImage(ev.ImagePath); rendered != "" {
						fmt.Print(rendered)
						fmt.Print("\r\n") // CR+LF to reset cursor position after image
						printedAny = true
						trailingNewlines = 1
					}
				}

			case ui.StreamEventDone:
				if len(pendingTools) > 0 {
					printTools()
				}
				ensureFinalSpacer()
				return nil

			case ui.StreamEventError:
				return ev.Err
			}
		}
	}
}

// getTerminalWidth returns the terminal width or a default
func getTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return 80 // default
	}
	return width
}

// askStreamModel is a bubbletea model for streaming ask responses
type askStreamModel struct {
	spinner spinner.Model
	styles  *ui.Styles
	width   int
	height  int

	// Tool and segment tracking (shared component)
	tracker *ui.ToolTracker

	// Smooth text buffer for word-by-word rendering (shared with chat)
	smoothBuffer      *ui.SmoothBuffer
	smoothTickPending bool

	// Subagent progress tracking
	subagentTracker *ui.SubagentTracker

	// State flags
	done bool // True when streaming is complete (prevents spinner from showing)

	// Status display
	retryStatus string    // Retry status (e.g., "Rate limited (2/5), waiting 5s...")
	startTime   time.Time // For elapsed time display
	totalTokens int       // Total tokens (input + output) used
	phase       string    // Current engine phase (Thinking, Searching, etc.)

	// External UI state
	pausedForExternalUI bool // True when paused for ask_user or approval prompts

	// Approval prompt state (using huh form)
	approvalForm       *huh.Form
	approvalDesc       string
	approvalToolInfo   string      // Info for the tool that triggered approval (to avoid duplicates)
	approvalResponseCh chan<- bool // channel to send y/n response back to tool

	// Session for inspector (to fetch messages)
	store session.Store
	sess  *session.Session

	// Inspector mode
	inspectorMode  bool
	inspectorModel *inspector.Model

	// View caching (performance optimization - Issue 4)
	// Avoids rebuilding content on every spinner tick
	contentDirty           bool   // True when new content arrived, needs re-render
	cachedContent          string // Cached rendered content from last dirty=true render
	adaptiveFlushThreshold bool   // Scales flush threshold with smooth buffer size when enabled

	// Tool-start boundary gating: defer pending tool-row rendering until
	// pre-tool completed content has been visibly flushed to scrollback.
	pendingBoundaryFlushes int
	pendingWaveStart       bool
}

const askAdaptiveFlushThresholdEnv = "TERM_LLM_ASK_ADAPTIVE_FLUSH"

const (
	askFlushBufferThresholdMedium = 400
	askFlushBufferThresholdLarge  = 1000
	askFlushBufferThresholdXLarge = 2000

	askFlushThresholdMedium = 32
	askFlushThresholdLarge  = 64
	askFlushThresholdXLarge = 128
)

type askContentMsg string
type askDoneMsg struct{}
type askCancelledMsg struct{}
type askUsageMsg struct {
	InputTokens  int
	OutputTokens int
}
type askTickMsg time.Time
type askToolStartMsg struct {
	CallID string // Unique ID for this tool invocation
	Name   string // Tool name being executed
	Info   string // Additional info (e.g., URL)
}
type askToolEndMsg struct {
	CallID  string // Unique ID for this tool invocation
	Success bool   // Whether the tool succeeded
}
type askRetryMsg struct {
	Attempt     int
	MaxAttempts int
	WaitSecs    float64
}
type askPhaseMsg string
type askImageMsg string // Image path to display
type askDiffMsg struct {
	Path string
	Old  string
	New  string
	Line int
}
type askBoundaryFlushedMsg struct {
	CallID string
	Name   string
}
type askFlushBeforeAskUserMsg struct {
	Done chan<- struct{} // Signal when flush is complete
}
type askFlushBeforeApprovalMsg struct {
	Done chan<- struct{} // Signal when flush is complete
}
type askResumeFromExternalUIMsg struct{}
type askApprovalRequestMsg struct {
	Description string
	ToolName    string
	ToolInfo    string
	ResponseCh  chan<- bool
}

// Subagent progress messages
type askSubagentProgressMsg struct {
	CallID string
	Event  tools.SubagentEvent
}

// Use ui.WaveTickMsg and ui.WavePauseMsg from the shared ToolTracker

func newAskStreamModel() askStreamModel {
	width := getTerminalWidth()
	height := 24
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		height = h
	}
	styles := ui.DefaultStyles()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styles.Spinner

	return askStreamModel{
		spinner:                s,
		styles:                 styles,
		width:                  width,
		height:                 height,
		tracker:                ui.NewToolTracker(),
		smoothBuffer:           ui.NewSmoothBuffer(),
		subagentTracker:        ui.NewSubagentTracker(),
		startTime:              time.Now(),
		adaptiveFlushThreshold: ui.ParseBoolDefault(os.Getenv(askAdaptiveFlushThresholdEnv), true),
	}
}

func (m *askStreamModel) streamingFlushThreshold() int {
	if !m.adaptiveFlushThreshold || m.smoothBuffer == nil {
		return 0
	}

	bufferLen := m.smoothBuffer.Len()
	// As buffered text grows, require larger safe chunks before flushing to
	// scrollback to reduce high-frequency partial render churn on long outputs.
	switch {
	case bufferLen >= askFlushBufferThresholdXLarge:
		return askFlushThresholdXLarge
	case bufferLen >= askFlushBufferThresholdLarge:
		return askFlushThresholdLarge
	case bufferLen >= askFlushBufferThresholdMedium:
		return askFlushThresholdMedium
	default:
		return 0
	}
}

func (m askStreamModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.tickEvery())
}

// tickEvery returns a command that sends a tick every second for elapsed time updates.
func (m askStreamModel) tickEvery() tea.Cmd {
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg {
		return askTickMsg(t)
	})
}

// maxViewLines is the maximum number of lines to keep in View().
// Content beyond this is printed to scrollback to prevent scroll issues.
const maxViewLines = 8

// maybeFlushToScrollback checks if there are segments to flush to scrollback,
// keeping View() small to avoid terminal scroll issues.
// Note: Only flushes images/diffs immediately; text is accumulated and printed at end.
func (m *askStreamModel) maybeFlushToScrollback() tea.Cmd {
	result := m.tracker.FlushToScrollback(m.width, 0, maxViewLines, renderMd)
	if result.ToPrint != "" {
		m.cachedContent = "" // Invalidate cache since segments were flushed
		m.contentDirty = true
		return tea.Printf("%s", result.ToPrint)
	}
	return nil
}

// flushCompletedBoundaryNow flushes all completed segments immediately.
// Use this at tool boundaries to preserve strict text/tool interleaving order.
func (m *askStreamModel) flushCompletedBoundaryNow() tea.Cmd {
	result := m.tracker.FlushCompletedNow(m.width, renderMd)
	if result.ToPrint != "" {
		m.cachedContent = "" // Invalidate cache since segments were flushed
		m.contentDirty = true
		return tea.Printf("%s", result.ToPrint)
	}
	return nil
}

func (m *askStreamModel) flushCompletedBoundaryNowWithAck(callID, name string) tea.Cmd {
	result := m.tracker.FlushCompletedNow(m.width, renderMd)
	if result.ToPrint != "" {
		m.cachedContent = "" // Invalidate cache since segments were flushed
		m.contentDirty = true
		return tea.Sequence(
			tea.Printf("%s", result.ToPrint),
			func() tea.Msg {
				return askBoundaryFlushedMsg{CallID: callID, Name: name}
			},
		)
	}
	return func() tea.Msg {
		return askBoundaryFlushedMsg{CallID: callID, Name: name}
	}
}

// flushSmoothBufferToTracker flushes any buffered words into the text tracker.
func (m *askStreamModel) flushSmoothBufferToTracker() {
	if m.smoothBuffer == nil {
		return
	}

	remaining := m.smoothBuffer.FlushAll()
	if remaining == "" {
		return
	}

	m.tracker.AddTextSegment(remaining, m.width)
	m.contentDirty = true
}

func (m askStreamModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle inspector mode
	if m.inspectorMode {
		return m.updateInspectorMode(msg)
	}

	// Handle tool start messages even while approval form is active
	if toolMsg, ok := msg.(askToolStartMsg); ok && m.approvalForm != nil {
		m.flushSmoothBufferToTracker()
		// Mark current text segment as complete before starting tool
		m.tracker.MarkCurrentTextComplete(func(text string) string {
			return renderMd(text, m.width)
		})
		if m.tracker.HandleToolStart(toolMsg.CallID, toolMsg.Name, toolMsg.Info) {
			// New segment added, but don't start wave yet (approval form is active)
		}
	}

	// If approval form is active, delegate to it
	if m.approvalForm != nil {
		form, cmd := m.approvalForm.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.approvalForm = f
		}

		// Check if form completed
		if m.approvalForm.State == huh.StateCompleted {
			approved := m.approvalForm.GetBool("confirm")
			// Send response - the tool segment will be updated by askToolEndMsg
			// when the tool actually completes (not when approval is granted)
			if m.approvalResponseCh != nil {
				m.approvalResponseCh <- approved
			}
			m.approvalForm = nil
			m.approvalResponseCh = nil
			m.approvalDesc = ""
			m.approvalToolInfo = ""
			// If there are pending tools, restart wave animation
			if m.tracker.HasPending() {
				return m, m.tracker.StartWave()
			}
			return m, m.spinner.Tick
		}

		// Check if form was aborted (esc/ctrl+c)
		if m.approvalForm.State == huh.StateAborted {
			if m.approvalResponseCh != nil {
				m.approvalResponseCh <- false
			}
			m.approvalForm = nil
			m.approvalResponseCh = nil
			m.approvalDesc = ""
			return m, tea.Quit
		}

		return m, cmd
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			return m, func() tea.Msg { return askCancelledMsg{} }
		}
		// Handle CTRL-O to open inspector (works during streaming too)
		if msg.String() == "ctrl+o" && m.store != nil && m.sess != nil {
			// Fetch messages from session store
			messages, err := m.store.GetMessages(context.Background(), m.sess.ID, 0, 0)
			if err != nil {
				// Log error but don't interrupt the user - just don't open inspector
				m.tracker.AddTextSegment(fmt.Sprintf("\n[Inspector error: %v]\n", err), m.width)
				return m, nil
			}
			if len(messages) > 0 {
				m.inspectorMode = true
				m.inspectorModel = inspector.NewWithStore(messages, m.width, m.height, m.styles, m.store)
				return m, tea.EnterAltScreen
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.cachedContent = ""  // Invalidate cache
		m.contentDirty = true // Width changed, need to re-render
		// Invalidate cached markdown renderings since they are width-dependent
		for i := range m.tracker.Segments {
			if m.tracker.Segments[i].Type == ui.SegmentText {
				m.tracker.Segments[i].Rendered = ""
				// Also clear streaming cache for in-progress segments
				// so next render recalculates with new width
				m.tracker.Segments[i].SafeRendered = ""
				m.tracker.Segments[i].SafePos = 0
			}
		}
		// Resize active streaming renderers
		m.tracker.ResizeStreamRenderers(m.width)

	case askContentMsg:
		// Buffer text and release it in smooth word-paced ticks.
		if m.smoothBuffer != nil {
			m.smoothBuffer.Write(string(msg))
			if !m.smoothTickPending {
				m.smoothTickPending = true
				return m, ui.SmoothTick()
			}
			return m, nil
		}
		// Fallback: direct append if smooth buffer is unavailable.
		m.tracker.AddTextSegment(string(msg), m.width)
		m.contentDirty = true

	case askDoneMsg:
		m.done = true // Prevent spinner from showing in final View()
		m.smoothTickPending = false
		// Ensure we have a valid width
		if m.width <= 0 {
			m.width = getTerminalWidth()
		}
		// Flush buffered words before final segment completion.
		m.flushSmoothBufferToTracker()
		if m.smoothBuffer != nil {
			m.smoothBuffer.MarkDone()
		}
		// Complete text segments (finalizes TextBuilder -> Text)
		m.tracker.CompleteTextSegments(func(text string) string {
			return renderMd(text, m.width)
		})

		// Force-complete any pending tools (defensive - shouldn't happen normally)
		m.tracker.ForceCompletePendingTools()

		// Flush everything remaining to scrollback
		res := m.tracker.FlushAllRemaining(m.width, 0, renderMd)
		if res.ToPrint != "" {
			return m, tea.Sequence(tea.Printf("%s", res.ToPrint), tea.Quit)
		}
		return m, tea.Quit

	case askCancelledMsg:
		m.done = true
		m.smoothTickPending = false
		// Ensure we have a valid width
		if m.width <= 0 {
			m.width = getTerminalWidth()
		}
		// Flush buffered words so cancellation keeps latest visible text.
		m.flushSmoothBufferToTracker()
		if m.smoothBuffer != nil {
			m.smoothBuffer.MarkDone()
		}
		// Flush whatever has been rendered so far (including partial text) to scrollback
		res := m.tracker.FlushAllRemaining(m.width, 0, renderMd)
		if res.ToPrint != "" {
			return m, tea.Sequence(tea.Printf("%s", res.ToPrint), tea.Quit)
		}
		return m, tea.Quit

	case ui.SmoothTickMsg:
		if m.smoothBuffer == nil || m.done {
			return m, nil
		}
		m.smoothTickPending = false

		var flushCmds []tea.Cmd
		var asyncCmds []tea.Cmd

		words := m.smoothBuffer.NextWords()
		if words != "" {
			m.tracker.AddTextSegment(words, m.width)
			m.contentDirty = true

			// Flush as soon as we have any safe boundary to avoid duplication/corruption.
			streamingFlushThreshold := m.streamingFlushThreshold()
			if m.width > 0 {
				result := m.tracker.FlushStreamingText(streamingFlushThreshold, m.width, renderMd)
				if result.ToPrint != "" {
					m.cachedContent = "" // Invalidate cache since state changed
					flushCmds = append(flushCmds, tea.Printf("%s", result.ToPrint))
				}
			}
		}

		if !m.smoothBuffer.IsDrained() {
			if !m.smoothTickPending {
				m.smoothTickPending = true
				asyncCmds = append(asyncCmds, ui.SmoothTick())
			}
		}

		cmd := ui.ComposeFlushFirstCommands(flushCmds, asyncCmds)
		if cmd == nil {
			return m, nil
		}
		return m, cmd

	case askUsageMsg:
		m.totalTokens = msg.InputTokens + msg.OutputTokens

	case askTickMsg:
		// Stop ticking when done
		if m.done {
			return m, nil
		}
		// Continue ticking for elapsed time and idle detection updates
		return m, m.tickEvery()

	case askRetryMsg:
		m.retryStatus = fmt.Sprintf("Rate limited (%d/%d), waiting %.0fs...",
			msg.Attempt, msg.MaxAttempts, msg.WaitSecs)
		return m, m.tickEvery()

	case askPhaseMsg:
		m.phase = string(msg)
		// Display WARNING phases as visible text in the conversation
		if strings.HasPrefix(string(msg), llm.WarningPhasePrefix) {
			m.tracker.AddTextSegment(string(msg)+"\n", m.width)
			m.contentDirty = true
		}
		return m, nil

	case askImageMsg:
		// Add image segment for inline display
		m.tracker.AddImageSegment(string(msg))

		// Flush to scrollback so image appears
		if cmd := m.maybeFlushToScrollback(); cmd != nil {
			return m, cmd
		}
		return m, nil

	case askDiffMsg:
		// Add diff segment for inline display
		m.tracker.AddDiffSegment(msg.Path, msg.Old, msg.New, msg.Line)
		// Flush to scrollback so diff appears
		if cmd := m.maybeFlushToScrollback(); cmd != nil {
			return m, cmd
		}
		return m, nil

	case askFlushBeforeAskUserMsg:
		// Set flag to suppress spinner in View() while external UI is active
		m.pausedForExternalUI = true
		m.flushSmoothBufferToTracker()

		// Partial flush - keep some context visible for after external UI returns
		result := m.tracker.FlushBeforeExternalUI(m.width, 0, maxViewLines, renderMd)

		var cmds []tea.Cmd
		if result.ToPrint != "" {
			cmds = append(cmds, tea.Printf("%s", result.ToPrint))
		}

		// Signal that flush is complete (use a command to ensure tea.Println finishes first)
		cmds = append(cmds, func() tea.Msg {
			close(msg.Done)
			return nil
		})
		return m, tea.Sequence(cmds...)

	case askFlushBeforeApprovalMsg:
		// Set flag to suppress spinner in View() while approval UI is active
		m.pausedForExternalUI = true
		m.flushSmoothBufferToTracker()

		// Partial flush - keep some context visible for after external UI returns
		result := m.tracker.FlushBeforeExternalUI(m.width, 0, maxViewLines, renderMd)

		var cmds []tea.Cmd
		if result.ToPrint != "" {
			cmds = append(cmds, tea.Printf("%s", result.ToPrint))
		}

		// Signal that flush is complete (use a command to ensure tea.Println finishes first)
		cmds = append(cmds, func() tea.Msg {
			close(msg.Done)
			return nil
		})
		return m, tea.Sequence(cmds...)

	case askResumeFromExternalUIMsg:
		// Resume from external UI (ask_user or approval)
		m.pausedForExternalUI = false

		// Check if there's an ask_user summary to display
		// Add to tracker so it appears in correct order, then flush
		if summary := tools.GetAndClearAskUserResult(); summary != "" {
			m.tracker.AddExternalUIResult(summary)
			if cmd := m.maybeFlushToScrollback(); cmd != nil {
				return m, ui.ComposeFlushFirstCommands([]tea.Cmd{cmd}, []tea.Cmd{m.spinner.Tick})
			}
		}

		return m, m.spinner.Tick

	case askToolStartMsg:
		m.retryStatus = ""
		m.flushSmoothBufferToTracker()
		m.smoothTickPending = false

		// Mark current text segment as complete before starting tool
		// This preserves interleaving order: text -> tool -> text
		m.tracker.MarkCurrentTextComplete(func(text string) string {
			return renderMd(text, m.width)
		})

		// Add or dedupe pending tool segment for this call.
		m.tracker.HandleToolStart(msg.CallID, msg.Name, msg.Info)

		// Defer pending tool rendering until boundary flush is visibly committed.
		m.pendingBoundaryFlushes++
		m.contentDirty = true

		// Start (or restart) wave animation for non-ask_user tools after the
		// boundary flush acknowledgement arrives.
		if msg.Name != tools.AskUserToolName {
			m.pendingWaveStart = true
		}
		return m, m.flushCompletedBoundaryNowWithAck(msg.CallID, msg.Name)

	case askBoundaryFlushedMsg:
		if m.pendingBoundaryFlushes > 0 {
			m.pendingBoundaryFlushes--
		}
		if m.pendingBoundaryFlushes > 0 {
			return m, nil
		}

		// Boundary is now visible in scrollback; refresh content and reveal tools.
		m.cachedContent = m.tracker.RenderUnflushed(m.width, renderMd, false)
		m.contentDirty = false

		if m.pendingWaveStart && m.tracker.HasPending() {
			m.pendingWaveStart = false
			return m, m.tracker.StartWave()
		}
		m.pendingWaveStart = false

	case askToolEndMsg:
		m.tracker.HandleToolEnd(msg.CallID, msg.Success)

		// Remove from subagent tracker when spawn_agent completes
		m.subagentTracker.Remove(msg.CallID)

		// Flush completed segments immediately at tool boundary.
		var flushCmd tea.Cmd
		if cmd := m.flushCompletedBoundaryNow(); cmd != nil {
			flushCmd = cmd
		}

		// Immediately rebuild cache with the remaining content
		m.cachedContent = m.tracker.RenderUnflushed(m.width, renderMd, false)

		if flushCmd != nil {
			return m, flushCmd
		}

		// If no more pending tools, start spinner for idle state
		if !m.tracker.HasPending() {
			return m, m.spinner.Tick
		}

	case askSubagentProgressMsg:
		// Handle subagent progress events and update segment stats
		ui.HandleSubagentProgress(m.tracker, m.subagentTracker, msg.CallID, msg.Event)

	case ui.WaveTickMsg:
		if cmd := m.tracker.HandleWaveTick(); cmd != nil {
			return m, cmd
		}

	case ui.WavePauseMsg:
		if cmd := m.tracker.HandleWavePause(); cmd != nil {
			return m, cmd
		}

	case askApprovalRequestMsg:
		m.approvalDesc = msg.Description
		m.approvalResponseCh = msg.ResponseCh
		m.approvalToolInfo = msg.ToolInfo

		// Don't add a new segment - the tool already has a pending segment from askToolStartMsg.
		// The approval is part of that tool's execution, not a separate operation.

		m.approvalForm = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Key("confirm").
					Title(msg.Description).
					Affirmative("Yes").
					Negative("No").
					WithButtonAlignment(lipgloss.Left),
			),
		).WithShowHelp(false).WithShowErrors(false)
		return m, m.approvalForm.Init()

	case spinner.TickMsg:
		// Always maintain the tick chain to keep spinner animating
		// The idle check in View() controls visibility, not animation
		// Don't update spinner when done
		if !m.done {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	// Update cached content when dirty (must be done in Update, not View)
	// View() uses a value receiver so mutations are discarded by Bubble Tea
	if m.contentDirty && m.pendingBoundaryFlushes == 0 {
		m.cachedContent = m.tracker.RenderUnflushed(m.width, renderMd, false)
		m.contentDirty = false
	}

	return m, nil
}

func renderMd(text string, width int) string {
	return ui.RenderMarkdown(text, width)
}

// updateInspectorMode handles updates while in inspector mode
func (m askStreamModel) updateInspectorMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Pass to inspector
		if m.inspectorModel != nil {
			m.inspectorModel, _ = m.inspectorModel.Update(msg)
		}
		return m, nil

	case tea.KeyMsg:
		// Pass to inspector
		if m.inspectorModel != nil {
			var cmd tea.Cmd
			m.inspectorModel, cmd = m.inspectorModel.Update(msg)
			return m, cmd
		}
		return m, nil

	case inspector.CloseMsg:
		// Exit inspector mode and return to stream UI
		m.inspectorMode = false
		m.inspectorModel = nil
		return m, tea.ExitAltScreen

	default:
		// Pass through to inspector
		if m.inspectorModel != nil {
			var cmd tea.Cmd
			m.inspectorModel, cmd = m.inspectorModel.Update(msg)
			return m, cmd
		}
	}

	// Update cached content when dirty (must be done in Update, not View)
	// View() uses a value receiver so mutations are discarded by Bubble Tea
	if m.contentDirty && m.pendingBoundaryFlushes == 0 {
		m.cachedContent = m.tracker.RenderUnflushed(m.width, renderMd, false)
		m.contentDirty = false
	}

	return m, nil
}

func (m askStreamModel) View() string {
	// Inspector mode uses alternate screen
	if m.inspectorMode && m.inspectorModel != nil {
		return m.inspectorModel.View()
	}

	// When done, return empty string so flushed content isn't duplicated
	if m.done {
		return ""
	}

	var b strings.Builder

	// Get segments from tracker (excludes flushed segments)
	active := m.tracker.ActiveSegments()
	if m.pendingBoundaryFlushes > 0 {
		active = nil
	}

	// Use cached content (updated in Update() when contentDirty was true)
	// Fall back to rendering on-demand if cache is empty but we have segments
	// (handles tests that call View() directly without Update())
	content := m.cachedContent
	if content == "" && m.pendingBoundaryFlushes == 0 {
		content = m.tracker.RenderUnflushed(m.width, renderMd, false)
	}
	if content != "" {
		b.WriteString(content)
	}

	// If approval form is active, show it after content (no spinner during approval)
	if m.approvalForm != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		} else {
			b.WriteString(m.tracker.FlushLeadingSeparator(ui.SegmentTool))
		}
		b.WriteString(m.approvalForm.View())
		return b.String()
	}

	// Show spinner when idle (no activity for >1s) or when tools are active
	// Don't show spinner when done or paused for external UI (ask_user/approval)
	if !m.done && !m.pausedForExternalUI && (len(active) > 0 || m.tracker.IsIdle(time.Second)) {
		hasContent := b.Len() > 0
		if hasContent {
			b.WriteString("\n")
		}
		phase := m.phase
		if phase == "" {
			phase = "Thinking"
		}

		indicator := ui.StreamingIndicator{
			Spinner:         m.spinner.View(),
			Phase:           phase,
			Elapsed:         time.Since(m.startTime),
			Tokens:          m.totalTokens,
			Status:          m.retryStatus,
			ShowCancel:      true,
			Segments:        active,
			WavePos:         m.tracker.WavePos,
			Width:           m.width,
			RenderMarkdown:  renderMd,
			HasFlushed:      !hasContent && m.tracker.HasFlushed,
			LastFlushedType: m.tracker.LastFlushedType,
		}.Render(m.styles)
		b.WriteString(indicator)
	}

	return b.String()
}

// streamWithGlamour renders markdown beautifully as content streams in
// If p is nil, creates a new tea.Program; otherwise uses the provided one.
func streamWithGlamour(ctx context.Context, events <-chan ui.StreamEvent, p *tea.Program, store session.Store, sess *session.Session) error {
	// Create program if not provided (when no tools are used)
	if p == nil {
		model := newAskStreamModel()
		model.store = store
		model.sess = sess
		p = tea.NewProgram(model, tea.WithoutSignalHandler())
	}

	type programResult struct {
		model tea.Model
		err   error
	}
	programDone := make(chan programResult, 1)
	go func() {
		m, err := p.Run()
		programDone <- programResult{model: m, err: err}
	}()

	var streamErr error
	for events != nil {
		select {
		case <-ctx.Done():
			p.Send(askCancelledMsg{})
			events = nil

		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}

			switch ev.Type {
			case ui.StreamEventRetry:
				p.Send(askRetryMsg{
					Attempt:     ev.RetryAttempt,
					MaxAttempts: ev.RetryMax,
					WaitSecs:    ev.RetryWait,
				})

			case ui.StreamEventUsage:
				p.Send(askUsageMsg{
					InputTokens:  ev.InputTokens,
					OutputTokens: ev.OutputTokens,
				})

			case ui.StreamEventPhase:
				p.Send(askPhaseMsg(ev.Phase))

			case ui.StreamEventToolEnd:
				p.Send(askToolEndMsg{
					CallID:  ev.ToolCallID,
					Success: ev.ToolSuccess,
				})

			case ui.StreamEventToolStart:
				p.Send(askToolStartMsg{
					CallID: ev.ToolCallID,
					Name:   ev.ToolName,
					Info:   ev.ToolInfo,
				})

			case ui.StreamEventText:
				p.Send(askContentMsg(ev.Text))

			case ui.StreamEventImage:
				p.Send(askImageMsg(ev.ImagePath))

			case ui.StreamEventDiff:
				p.Send(askDiffMsg{
					Path: ev.DiffPath,
					Old:  ev.DiffOld,
					New:  ev.DiffNew,
					Line: ev.DiffLine,
				})

			case ui.StreamEventDone:
				p.Send(askDoneMsg{})
				events = nil

			case ui.StreamEventError:
				if ev.Err != nil {
					streamErr = ev.Err
					p.Send(askCancelledMsg{})
					events = nil
				}
			}
		}
	}

	result := <-programDone

	if streamErr != nil {
		return streamErr
	}
	return result.err
}

// MCPOptions contains options for enabling MCP servers.
type MCPOptions struct {
	Provider llm.Provider
	Model    string
	YoloMode bool
}

// enableMCPServersWithFeedback initializes MCP servers with user feedback.
// Returns the manager (caller must call StopAll) or error if setup failed.
func enableMCPServersWithFeedback(ctx context.Context, mcpFlag string, engine *llm.Engine, errWriter io.Writer, opts *MCPOptions) (*mcp.Manager, error) {
	serverNames := parseServerList(mcpFlag)
	if len(serverNames) == 0 {
		return nil, nil
	}

	mcpManager := mcp.NewManager()
	if err := mcpManager.LoadConfig(); err != nil {
		return nil, fmt.Errorf("failed to load MCP config: %w", err)
	}

	// Set up sampling handler if provider is available
	if opts != nil && opts.Provider != nil {
		mcpManager.SetSamplingProvider(opts.Provider, opts.Model, opts.YoloMode)
	}

	// Validate all servers exist before starting any
	available := mcpManager.AvailableServers()
	availableSet := make(map[string]bool)
	for _, s := range available {
		availableSet[s] = true
	}

	var missing []string
	for _, name := range serverNames {
		if !availableSet[name] {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		if len(missing) == 1 {
			return nil, fmt.Errorf("MCP server '%s' not configured. Add it with: term-llm mcp add %s", missing[0], missing[0])
		}
		return nil, fmt.Errorf("MCP servers not configured: %s. Add them with: term-llm mcp add <name>", strings.Join(missing, ", "))
	}

	// Show starting message
	fmt.Fprintf(errWriter, "Starting MCP: %s", strings.Join(serverNames, ", "))

	// Enable all servers (async)
	var enableErrors []string
	for _, server := range serverNames {
		if err := mcpManager.Enable(ctx, server); err != nil {
			enableErrors = append(enableErrors, fmt.Sprintf("%s: %v", server, err))
		}
	}

	if len(enableErrors) > 0 {
		fmt.Fprintf(errWriter, "\n")
		return nil, fmt.Errorf("failed to start MCP servers: %s", strings.Join(enableErrors, "; "))
	}

	// Wait for servers with spinner animation
	spinChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinIdx := 0
	timeout := 10 * time.Second
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		allReady := true
		for _, name := range mcpManager.EnabledServers() {
			status, _ := mcpManager.ServerStatus(name)
			if status == mcp.StatusStarting {
				allReady = false
				break
			}
		}
		if allReady {
			break
		}
		fmt.Fprintf(errWriter, "\r%s Starting MCP: %s", spinChars[spinIdx], strings.Join(serverNames, ", "))
		spinIdx = (spinIdx + 1) % len(spinChars)
		time.Sleep(80 * time.Millisecond)
	}

	// Check for failed servers
	var failedServers []string
	for _, name := range serverNames {
		status, err := mcpManager.ServerStatus(name)
		if status == mcp.StatusFailed {
			errMsg := "unknown error"
			if err != nil {
				errMsg = err.Error()
			}
			failedServers = append(failedServers, fmt.Sprintf("%s (%s)", name, errMsg))
		}
	}

	if len(failedServers) > 0 {
		fmt.Fprintf(errWriter, "\n")
		return nil, fmt.Errorf("MCP servers failed to start: %s", strings.Join(failedServers, "; "))
	}

	// Register MCP tools
	mcp.RegisterMCPTools(mcpManager, engine.Tools())
	tools := mcpManager.AllTools()

	// Show result
	if len(tools) > 0 {
		fmt.Fprintf(errWriter, "\r✓ MCP ready: %d tools from %s\n\n", len(tools), strings.Join(serverNames, ", "))
	} else {
		fmt.Fprintf(errWriter, "\n")
		return nil, fmt.Errorf("MCP servers started but no tools available from: %s", strings.Join(serverNames, ", "))
	}

	return mcpManager, nil
}

// parseServerList splits comma-separated server names and trims whitespace.
func parseServerList(mcpFlag string) []string {
	var servers []string
	for s := range strings.SplitSeq(mcpFlag, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			servers = append(servers, s)
		}
	}
	return servers
}

// writeCommitEditMsg writes the commit message to .git/COMMIT_EDITMSG and .git/GITGUI_MSG.
func writeCommitEditMsg(message string) error {
	gitInfo := tools.DetectGitRepo(".")
	if !gitInfo.IsRepo {
		return fmt.Errorf("not in a git repository")
	}
	message = strings.TrimSpace(message) + "\n"
	data := []byte(message)

	// Write to both files - COMMIT_EDITMSG for git commit, GITGUI_MSG for git gui
	commitPath := filepath.Join(gitInfo.Root, ".git", "COMMIT_EDITMSG")
	guiPath := filepath.Join(gitInfo.Root, ".git", "GITGUI_MSG")

	if err := os.WriteFile(commitPath, data, 0644); err != nil {
		return err
	}
	return os.WriteFile(guiPath, data, 0644)
}

// runOnComplete executes the on_complete shell command with input piped to stdin.
// Runs in the git repo root if available, else cwd.
func runOnComplete(command, input string) error {
	// Run in git repo root if available, else cwd
	dir := "."
	if gitInfo := tools.DetectGitRepo("."); gitInfo.IsRepo {
		dir = gitInfo.Root
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// textCollector wraps an event channel and collects all text events.
type textCollector struct {
	text strings.Builder
}

// wrapEventsWithCollector creates a new channel that forwards all events from the input
// channel while collecting text events. Returns the wrapped channel.
func (tc *textCollector) wrapEvents(events <-chan ui.StreamEvent) <-chan ui.StreamEvent {
	wrapped := make(chan ui.StreamEvent, 100)
	go func() {
		defer close(wrapped)
		for ev := range events {
			if ev.Type == ui.StreamEventText {
				tc.text.WriteString(ev.Text)
			}
			wrapped <- ev
		}
	}()
	return wrapped
}

// Text returns the collected text.
func (tc *textCollector) Text() string {
	return tc.text.String()
}
