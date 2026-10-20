package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/app/ask"
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/app/runtime/progressive"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/mcp"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	askDebug           bool
	askSearch          bool
	askNoSearch        bool
	askText            bool
	askPorcelain       bool
	askJSON            bool
	askProgressive     bool
	askProvider        string
	askFiles           []string
	askMCP             string
	askMaxTurns        int
	askMaxOutputTokens int
	askInlineMaxBytes  string
	askTimeout         time.Duration
	askStopWhen        string
	askContinueWith    string
	askNativeSearch    bool
	askNoNativeSearch  bool
	askNoWebFetch      bool
	// Tool flags
	askTools         string
	askReadDirs      []string
	askWriteDirs     []string
	askShellAllow    []string
	askSystemMessage string
	// Agent flag
	askAgent string
	// Approval modes
	askApproval string
	askYolo     bool
	askAuto     bool
	// Fast provider flag
	askFast bool
	// Skills flag
	askSkills string
	// Session resume flag
	askResume string

	askRunnerCleanupTimeout = runpkg.DefaultRunnerCleanupTimeout

	newAskProvider = func(cfg *config.Config, fast bool) (core.Provider, error) {
		if fast {
			key, model, ok := llm.ResolveFastTarget(cfg, cfg.DefaultProvider)
			if !ok {
				return nil, fmt.Errorf("no fast provider configured for %q", cfg.DefaultProvider)
			}
			return llm.NewProviderByName(cfg, key, model)
		}
		return llm.NewProvider(cfg)
	}
)

func shouldUseAskRichRenderer() bool {
	// The renderer owns keyboard input for cancellation as well as stdout. Do
	// not let Bubble Tea recover input through a controlling TTY when stdin was
	// deliberately redirected or captured.
	return !askText && !debugRaw && terminalpolicy.Interactive(os.Stdin, os.Stdout)
}

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
  term-llm ask "Explain git rebase" --json | jq -c .
  term-llm ask -f code.go "Explain this code"
  term-llm ask -f code.go:10-50 "Explain this function"
  term-llm ask -f clipboard "What is this?"
  cat error.log | term-llm ask "What went wrong?"

Agent examples (use @agent shortcut or --agent flag):
  term-llm ask @reviewer "Review this code" -f main.go
  term-llm ask @commit-message              (uses default prompt)
  term-llm ask @commit-message "focus on the bug fix"
  term-llm ask @editor "Add error handling" -f utils.go
  term-llm ask --agent web-researcher "Find info about Go 1.22"

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
	AddCommonFlags(askCmd,
		CommonCoreFlags|CommonSearchFlags|CommonMaxTurns|CommonMaxOutputTokens|CommonFiles|CommonAgent|CommonSkills,
		CommonFlagBindings{
			Provider:         &askProvider,
			Debug:            &askDebug,
			Search:           &askSearch,
			NoSearch:         &askNoSearch,
			NativeSearch:     &askNativeSearch,
			NoNativeSearch:   &askNoNativeSearch,
			NoWebFetch:       &askNoWebFetch,
			MCP:              &askMCP,
			MaxTurns:         &askMaxTurns,
			MaxTurnsDefault:  50,
			MaxOutputTokens:  &askMaxOutputTokens,
			Tools:            &askTools,
			ReadDirs:         &askReadDirs,
			WriteDirs:        &askWriteDirs,
			ShellAllow:       &askShellAllow,
			SystemMessage:    &askSystemMessage,
			Files:            &askFiles,
			FilesDescription: "File(s) to include as context (supports globs, line ranges like file.go:10-20, 'clipboard')",
			Agent:            &askAgent,
			Approval:         &askApproval,
			Yolo:             &askYolo,
			Auto:             &askAuto,
			Skills:           &askSkills,
		})

	// Ask-specific flags
	askCmd.Flags().BoolVarP(&askText, "text", "t", false, "Output plain text instead of rendered markdown")
	askCmd.Flags().BoolVar(&askPorcelain, "porcelain", false, "Output plain text without tool status lines (implies --text)")
	askCmd.Flags().BoolVar(&askJSON, "json", false, "Emit JSONL event stream on stdout (one event per line, implies --text)")
	askCmd.Flags().BoolVar(&askProgressive, "progressive", false, "Enable progressive execution with persisted best-so-far progress")
	askCmd.Flags().DurationVar(&askTimeout, "timeout", 0, "Set a hard deadline for the run (used by progressive execution for finalization budget)")
	askCmd.Flags().StringVar(&askStopWhen, "stop-when", "", "Progressive stop condition: done or timeout (defaults to done in progressive mode)")
	askCmd.Flags().StringVar(&askContinueWith, "continue-with", "", "Custom continuation prompt for progressive timeout mode")
	askCmd.Flags().BoolVar(&askFast, "fast", false, "Use the configured fast provider/model instead of the default")
	askCmd.Flags().StringVar(&askInlineMaxBytes, "inline-max-bytes", "", "Embed stdin/-f text up to this size inline instead of staging it for tools (e.g. 512K, 2M; overrides ask.stdin_inline_max_bytes)")

	// Session resume flag - NoOptDefVal allows --resume without a value
	askCmd.Flags().StringVarP(&askResume, "resume", "r", "", "Continue a session (empty for most recent, or session ID)")
	askCmd.Flags().Lookup("resume").NoOptDefVal = " " // space means "flag was passed without value"

	rootCmd.AddCommand(askCmd)
}

func runAsk(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext()
	defer stop()
	opts, deps := captureAskInvocation(cmd)
	return runAskWith(ctx, args, opts, deps)
}

func captureAskInvocation(cmd *cobra.Command) (ask.Options, ask.Dependencies) {
	opts := ask.Options{
		Debug:           askDebug,
		Search:          askSearch,
		NoSearch:        askNoSearch,
		Text:            askText,
		Porcelain:       askPorcelain,
		JSON:            askJSON,
		Progressive:     askProgressive,
		Provider:        askProvider,
		Files:           append([]string(nil), askFiles...),
		MCP:             askMCP,
		MaxTurns:        askMaxTurns,
		MaxOutputTokens: askMaxOutputTokens,
		InlineMaxBytes:  askInlineMaxBytes,
		Timeout:         askTimeout,
		StopWhen:        askStopWhen,
		ContinueWith:    askContinueWith,
		NativeSearch:    askNativeSearch,
		NoNativeSearch:  askNoNativeSearch,
		NoWebFetch:      askNoWebFetch,
		Tools:           askTools,
		ReadDirs:        append([]string(nil), askReadDirs...),
		WriteDirs:       append([]string(nil), askWriteDirs...),
		ShellAllow:      append([]string(nil), askShellAllow...),
		SystemMessage:   askSystemMessage,
		Agent:           askAgent,
		Approval:        askApproval,
		Yolo:            askYolo,
		Auto:            askAuto,
		Fast:            askFast,
		Skills:          askSkills,
		Resume:          askResume,
		DebugRaw:        debugRaw, ShowStats: showStats,
		Changed: ask.FlagState{
			Resume: cmd.Flags().Changed("resume"), Tools: cmd.Flags().Changed("tools"),
			System: cmd.Flags().Changed("system"), MaxTurns: cmd.Flags().Changed("max-turns"),
			Search: cmd.Flags().Changed("search"), MCP: cmd.Flags().Changed("mcp"),
		},
	}
	stdin, hasStdin := ask.CommandInput(cmd)
	// Capture CLI arbitration now, but defer errors/config resolution to startup.
	cli, cliErr := approvalModeFromCommand(cmd, opts.Approval, opts.Auto, opts.Yolo)
	deps := ask.Dependencies{
		Stdin: stdin, HasStdin: hasStdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
		Interactive:  func() bool { return terminalpolicy.Interactive(os.Stdin, os.Stdout) },
		SessionStore: sessionStoreOverrides(), MemoryPaths: memoryPaths(),
		LoadConfig: loadConfigWithSetup, NewProvider: newAskProvider, CreateDebugLogger: createDebugLogger,
		RunnerCleanupTimeout: askRunnerCleanupTimeout,
		ResolveApproval: func(cfg *config.Config) (resolvedApprovalMode, error) {
			if cliErr != nil {
				return resolvedApprovalMode{}, cliErr
			}
			return resolveApprovalMode(approvalModeResolutionInput{Surface: approvalSurfaceAsk, Config: cfg, CLI: cli})
		},
	}
	return opts, deps
}

func runAskWith(ctx context.Context, args []string, opts ask.Options, deps ask.Dependencies) error {
	// Extract @agent from args if present
	atAgent, filteredArgs := ExtractAgentFromArgs(args)
	if atAgent != "" && opts.Agent == "" {
		opts.Agent = atAgent
	}

	stdinReader, hasStdin := deps.Stdin, deps.HasStdin
	question := strings.Join(filteredArgs, " ")
	progressiveOpts := askProgressiveOptions{
		Enabled:      opts.Progressive,
		Timeout:      opts.Timeout,
		StopWhen:     progressiveStopWhen(strings.TrimSpace(opts.StopWhen)),
		ContinueWith: opts.ContinueWith,
	}
	if err := validateAskProgressiveOptions(&progressiveOpts); err != nil {
		return err
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cfg, err := deps.LoadConfig()
	if err != nil {
		return err
	}
	resolvedApproval, err := deps.ResolveApproval(cfg)
	if err != nil {
		return err
	}
	resolvedYolo := resolvedApproval.Mode == tools.ModeYolo
	opts.Approval = resolvedApproval.Mode.String()
	opts.Yolo = resolvedYolo
	opts.Auto = resolvedApproval.Mode == tools.ModeAuto

	// Load agent if specified
	agent, err := LoadAgent(opts.Agent, cfg)
	if err != nil {
		return err
	}

	// Handle default prompt for agents invoked without a message.
	// Allow empty question when stdin is piped (content comes from stdin).
	if question == "" && !hasStdin {
		if agent == nil {
			return fmt.Errorf("question required (or use @agent with a default prompt)")
		}
		if agent.DefaultPrompt == "" {
			return fmt.Errorf("agent %q has no default prompt; provide a question", agent.Name)
		}
		question = agent.DefaultPrompt
	}

	// Apply provider overrides: CLI > agent > config
	askSelected, askPolicy, err := selectRunModel(cfg, modelSelectionInput{Agent: agent, CmdProvider: cfg.Ask.Provider, CmdModel: cfg.Ask.Model, ProviderFlag: opts.Provider, Fast: opts.Fast})
	if err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := deps.NewProvider(cfg, opts.Fast)
	if err != nil {
		return err
	}
	engine := newEngine(provider, cfg)

	// Set up debug logger if enabled
	debugLogger, err := deps.CreateDebugLogger(cfg)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "warning: %v\n", err)
	}
	if debugLogger != nil {
		engine.SetDebugLogger(debugLogger)
		defer debugLogger.Close()
	}

	// Initialize skills system
	agentSkills := ""
	if agent != nil {
		agentSkills = agent.Skills
	}
	skillsSetup := appserve.SetupSkills(&cfg.Skills, opts.Skills, agentSkills, deps.Stderr)

	// Durable identity must be known before expanding the two refreshable
	// inputs. In particular, includes must not resolve against the launch CWD.
	var deferredInputs *appserve.SessionInputSelection
	if opts.Changed.Resume {
		deferredInputs = &appserve.SessionInputSelection{}
	}
	// Resolve all settings: CLI > agent > config
	runtimeSessionStore, runtimeMemoryPaths := deps.SessionStore, deps.MemoryPaths
	promptProvider, promptModel := appruntime.ActiveLLMFlags(cfg)
	settings, err := appserve.ResolveSettings(cfg, agent, appserve.CLIFlags{
		Inputs:           deferredInputs,
		Provider:         opts.Provider,
		ActiveProvider:   promptProvider,
		ActiveModel:      promptModel,
		ToolsSet:         opts.Changed.Tools,
		SystemMessageSet: opts.Changed.System,
		Tools:            opts.Tools,
		ReadDirs:         opts.ReadDirs,
		WriteDirs:        opts.WriteDirs,
		ShellAllow:       opts.ShellAllow,
		MCP:              opts.MCP,
		SystemMessage:    opts.SystemMessage,
		MaxTurns:         opts.MaxTurns,
		MaxTurnsSet:      opts.Changed.MaxTurns,
		MaxOutputTokens:  opts.MaxOutputTokens,
		Search:           opts.Search,
		NoSearch:         opts.NoSearch,
		Files:            opts.Files,
		Platform:         "console",
		MemoryPaths:      runtimeMemoryPaths,
	}, cfg.Ask.Provider, cfg.Ask.Model, cfg.Ask.Instructions, cfg.Ask.MaxTurns, 50)
	if err != nil {
		return err
	}
	settings.PrimaryWorkspace = settings.BaseDir

	// Initialize session store and handle --resume BEFORE tool/MCP initialization
	// so that session settings can override settings.Tools, settings.MCP, etc.
	store, storeCleanup := appruntime.InitSessionStoreWith(cfg, deps.Stderr, runtimeSessionStore)
	var spawnRunner *appserve.SpawnAgentRunner
	// userInterrupted is set when the rich renderer handled Ctrl-C itself: the
	// signal context is not cancelled in that case, but the user still asked
	// to stop, so outstanding children are interrupted instead of drained.
	userInterrupted := false
	defer func() {
		if spawnRunner != nil {
			ids := spawnRunner.OutstandingAgentIDs()
			switch {
			case ctx.Err() != nil || userInterrupted:
				if len(ids) > 0 {
					fmt.Fprintf(deps.Stderr, "interrupting child agents (resumable): %s\n", strings.Join(ids, ", "))
				}
				appserve.ShutdownSpawnAgentRunner(spawnRunner)
			default:
				if len(ids) > 0 {
					fmt.Fprintf(deps.Stderr, "waiting for child agents: %s\n", strings.Join(ids, ", "))
				}
				if err := spawnRunner.Drain(ctx); err != nil {
					appserve.ShutdownSpawnAgentRunner(spawnRunner)
				}
			}
		}
		storeCleanup()
	}()
	// Apply persisted settings and refreshable inputs before tool/MCP setup.
	sess, inputTicket, selectedInputs, resuming, err := prepareAskResume(ctx, opts, deps.Stderr, cfg, agent, store, &settings)
	if err != nil {
		return err
	}
	if sess != nil {
		// The model was selected above; a resumed session only adds its persisted
		// inherited rules and the rule of the agent it was created for.
		askPolicy = modelPolicyForSession(sess, agent)
		if sessAgent := strings.TrimSpace(sess.Agent); sessAgent != "" && (agent == nil || sessAgent != agent.Name) {
			resumedAgent, err := loadPersistedAgent(sessAgent, cfg)
			if err != nil {
				return err
			}
			if resumedAgent != nil {
				askPolicy = askPolicy.With(resumedAgent.Name, resumedAgent.AllowedModels)
			}
		}
		agentName := ""
		if agent != nil {
			agentName = agent.Name
		}
		if err := askPolicy.CheckWithConfig(cfg, agentName, askSelected.Provider, askSelected.Model); err != nil {
			return err
		}
	}
	if inputTicket != nil {
		defer inputTicket.Fail()
	}

	defaultQuestion := ""
	if agent != nil {
		defaultQuestion = agent.DefaultPrompt
	}
	askInputCfg, err := ask.ApplyInlineMaxBytesOverride(cfg.Ask, opts.InlineMaxBytes)
	if err != nil {
		return err
	}
	preparedInput, err := ask.PrepareInput(question, opts.Files, stdinReader, hasStdin, askInputCfg, defaultQuestion)
	if err != nil {
		return err
	}
	question = preparedInput.Question
	keepStagedInput := false
	defer func() {
		if !keepStagedInput {
			ask.CleanupPreparedInput(preparedInput)
		}
	}()
	settings.AutoTools = ask.AddAutoTools(settings.AutoTools, []core.Message{preparedInput.Message})

	// Resolve session identity before tool wiring captures it.
	sessionID, baseInputPrompt := finalizeAskSessionSettings(&settings, sess, selectedInputs, skillsSetup, cfg, provider, resuming)

	// Initialize local tools if we have any
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		return err
	}
	if toolMgr != nil && toolMgr.ApprovalMgr != nil {
		defer toolMgr.ApprovalMgr.Close()
	}
	spawnRunner, err = configureAskToolManager(ctx, deps.Stderr, opts.Debug, cfg, toolMgr, store, sessionID, resolvedApproval, runtimeSessionStore, runtimeMemoryPaths)
	if err != nil {
		return err
	}
	wireStaticAgentModelAdmission(cfg, toolMgr, spawnRunner, askPolicy, askSelected)
	var outputTool *tools.SetOutputTool

	if agent != nil && agent.OutputTool.IsConfigured() {
		outputTool = registerAgentOutputTool(agent.OutputTool, toolMgr, engine)
	}

	appserve.RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	// Initialize MCP servers if any (after session settings are applied)
	var mcpManager *mcp.Manager
	if settings.MCP != "" {
		mcpOpts := &MCPOptions{
			Provider:      provider,
			YoloMode:      resolvedYolo,
			ToolDiscovery: cfg.ToolDiscovery,
		}
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			mcpOpts.Model = providerCfg.Model
		}
		mcpManager, err = ask.EnableMCPServersWithFeedback(ctx, settings.MCP, engine, deps.Stderr, mcpOpts)
		if err != nil {
			return err
		}
		if mcpManager != nil {
			defer mcpManager.StopAll()
		}
	}

	preparedConversation, err := prepareAskConversation(ctx, cfg, provider, agent, store, sess, sessionID, resuming, settings, inputTicket, baseInputPrompt, preparedInput.Message, resolvedApproval.Mode)
	if err != nil {
		return err
	}
	sess, sessionID, settings = preparedConversation.session, preparedConversation.sessionID, preparedConversation.settings
	messages, instructions := preparedConversation.messages, preparedConversation.instructions
	historyHasSystem, conversationStartedAt := preparedConversation.historyHasSystem, preparedConversation.startedAt
	// Restore exact-file grants from structured parts only. On resume, scan the
	// durable full history as well as the active prepared conversation so grants
	// survive compaction boundaries without trusting text or escaped paths.
	grantAskResumedUploadedFileReads(ctx, store, sess, resuming, toolMgr, messages)

	debugMode := opts.Debug
	if sess != nil {
		sessionID = sess.ID
	}
	req := buildAskRequest(opts, cfg, settings, sessionID, messages, engine, toolMgr, mcpManager, outputTool, debugMode, opts.DebugRaw)

	// Check if we're in a TTY and can use terminal markdown rendering
	if opts.Porcelain {
		opts.Text = true
	}
	if opts.JSON {
		opts.Text = true
		opts.Porcelain = false
	}
	useRichRenderer := !opts.Text && !opts.DebugRaw && deps.Interactive()
	var jsonEmit *ask.JSONEmitter
	if opts.JSON {
		jsonEmit = ask.NewJSONEmitter(deps.Stdout)
	}

	// Create stream adapter for unified event handling with proper buffering
	adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
	if sess != nil {
		adapter.Stats().SeedTotals(sess.InputTokens, sess.OutputTokens, sess.CachedInputTokens, sess.CacheWriteTokens, sess.ToolCalls, sess.LLMTurns+sess.CompactionCount)
	}
	adapter.Stats().SetModel(activeModel(cfg))
	if !opts.Progressive && toolMgr != nil {
		toolMgr.ApprovalMgr.GuardianEventFunc = func(event tools.GuardianEvent) {
			if !event.Usage.BillableCountersZero() && store != nil && sess != nil {
				recordStoreHelperUsage(context.Background(), store, sess.ID, event.Model, session.ModelUsageGuardian, event.Usage)
			}
			adapter.EmitGuardian(ctx, event)
		}
	}

	var outputToolMessagesMu sync.Mutex
	outputToolMessages := append([]core.Message{}, messages...)
	outputToolFinalizationReq := func() core.Request {
		out := req
		outputToolMessagesMu.Lock()
		out.Messages = append([]core.Message{}, outputToolMessages...)
		outputToolMessagesMu.Unlock()
		return out
	}

	// Set up text collection for output capture (output_tool finalization,
	// commit_editmsg, on_complete, or session save).
	var collector *ask.TextCollector
	needsCollector := outputTool != nil || (agent != nil && (agent.Output == "commit_editmsg" || agent.OnComplete != "")) || (store != nil && sess != nil)
	wrapStreamEvents := func(events <-chan ui.StreamEvent) <-chan ui.StreamEvent {
		if !needsCollector || events == nil {
			return events
		}
		collector = &ask.TextCollector{}
		return collector.WrapEvents(events)
	}
	streamEvents := adapter.Events()

	if !useRichRenderer && toolMgr != nil {
		// Non-TUI mode: set up approval UI directly (no tea.Program to pause)
		toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (tools.ApprovalResult, error) {
			if isShell {
				return tools.RunShellApprovalUI(path, workDir)
			}
			return tools.RunFileApprovalUI(path, isWrite)
		}
		toolMgr.ApprovalMgr.WorkspacePromptFunc = tools.RunWorkspaceApprovalUI
	}

	// Save the initial transcript and construct callbacks before streaming.
	turnStartTime := time.Now()
	var compactionUsages compactionUsageCollector
	persistenceCallbacks := ask.PrepareAskPersistenceCallbacks(
		ctx, cfg, store, &sess, engine, &turnStartTime, instructions, historyHasSystem,
		resuming, messages, conversationStartedAt, preparedInput.Message, question, ask.CompactionHooks{Record: compactionUsages.add},
	)
	if persistenceCallbacks.Persistence != nil && persistenceCallbacks.Persistence.InitialUserPersisted {
		keepStagedInput = true
	}
	persistenceCallbacks.WrapOutputTool(outputTool != nil, &outputToolMessages, &outputToolMessagesMu)
	askPersistence := persistenceCallbacks.Persistence
	responseCompletedCallback := persistenceCallbacks.ResponseCompleted
	turnCompletedCallback := persistenceCallbacks.TurnCompleted
	persistSyntheticUserMessage := persistenceCallbacks.SyntheticUser
	assistantSnapshotCallback := persistenceCallbacks.AssistantSnapshot
	compactionCallback := persistenceCallbacks.Compaction

	var jsonInfo ask.SessionInfo
	if opts.JSON {
		agentName := ""
		if agent != nil {
			agentName = agent.Name
		}
		jsonInfo = ask.SessionInfo{
			SessionID: sessionID,
			Provider:  provider.Name(),
			Model:     activeModel(cfg),
			Agent:     agentName,
			Tools:     settings.Tools,
			MCP:       settings.MCP,
			Yolo:      resolvedYolo,
			Search:    settings.Search,
			Resuming:  resuming,
		}
	}

	var stats *ui.SessionStats
	var progressiveResult progressiveRunResult
	jsonTotalTokens := 0
	jsonFinalPending := false

	var parentApprovalMgr *tools.ApprovalManager
	if toolMgr != nil {
		parentApprovalMgr = toolMgr.ApprovalMgr
	}
	askRunner := appserve.NewCmdRunner(cfg, appserve.RunnerOptions{
		Provider:           opts.Provider,
		Fast:               opts.Fast,
		ConfigSet:          true,
		ConfigProvider:     cfg.Ask.Provider,
		ConfigModel:        cfg.Ask.Model,
		ConfigInstructions: cfg.Ask.Instructions,
		ConfigMaxTurns:     cfg.Ask.MaxTurns,
		Tools:              settings.Tools,
		ReadDirs:           append([]string(nil), opts.ReadDirs...),
		WriteDirs:          append([]string(nil), opts.WriteDirs...),
		ShellAllow:         append([]string(nil), opts.ShellAllow...),
		MCP:                settings.MCP,
		MaxTurns:           settings.MaxTurns,
		DefaultMaxTurns:    50,
		Search:             settings.Search,
		NoSearch:           opts.NoSearch,
		NativeSearch:       opts.NativeSearch,
		NoNativeSearch:     opts.NoNativeSearch,
		ApprovalMode:       resolvedApproval.Mode,
		ApprovalModeSet:    true,
		ApprovalSource:     resolvedApproval.Source,
		Debug:              opts.Debug,
		DebugRaw:           opts.DebugRaw,
		ErrWriter:          deps.Stderr,
		Store:              store,
		SessionStore:       runtimeSessionStore,
		MemoryPaths:        runtimeMemoryPaths,
		ParentApprovalMgr:  parentApprovalMgr,
	})
	includeConfiguredTools := false
	searchEnabled := settings.Search
	forceExternalSearch := req.ForceExternalSearch
	contextEstimateTotal, contextEstimateCount := 0, 0
	if sess != nil {
		contextEstimateTotal = sess.LastTotalTokens
		contextEstimateCount = sess.LastMessageCount
	}
	baseRunReq := runpkg.Request{
		Platform:                    runpkg.PlatformConsole,
		AgentName:                   opts.Agent,
		Messages:                    messages,
		Engine:                      engine,
		ProviderInstance:            provider,
		SessionID:                   sessionID,
		DeferSession:                true,
		DisableRuntimePersistence:   true,
		Provider:                    opts.Provider,
		Model:                       activeModel(cfg),
		Tools:                       settings.Tools,
		ReadDirs:                    append([]string(nil), opts.ReadDirs...),
		WriteDirs:                   append([]string(nil), opts.WriteDirs...),
		ShellAllow:                  append([]string(nil), opts.ShellAllow...),
		MCP:                         settings.MCP,
		Skills:                      opts.Skills,
		SystemMessage:               instructions,
		MaxTurns:                    settings.MaxTurns,
		MaxTurnsSet:                 true,
		MaxOutputTokens:             settings.MaxOutputTokens,
		Search:                      &searchEnabled,
		Debug:                       debugMode,
		DebugRaw:                    opts.DebugRaw,
		ForceExternalSearch:         &forceExternalSearch,
		DisableExternalWebFetch:     opts.NoWebFetch,
		ExtraTools:                  append([]core.ToolSpec(nil), req.Tools...),
		IncludeConfiguredTools:      &includeConfiguredTools,
		EnableToolDiscovery:         req.EnableToolDiscovery,
		OnAssistantSnapshot:         assistantSnapshotCallback,
		OnResponseCompleted:         responseCompletedCallback,
		OnTurnCompleted:             turnCompletedCallback,
		OnCompaction:                compactionCallback,
		OnSyntheticUserMessage:      persistSyntheticUserMessage,
		ContextEstimateTotalTokens:  contextEstimateTotal,
		ContextEstimateMessageCount: contextEstimateCount,
	}
	applyRunResult := func(result runpkg.Result) {
		if result.ProviderInstance != nil {
			provider = result.ProviderInstance
		}
		if result.Engine == nil {
			return
		}
		engine = result.Engine
		if outputTool != nil {
			if tool, ok := result.Engine.Tools().Get(outputTool.Name()); ok {
				if capturedOutput, ok := tool.(*tools.SetOutputTool); ok {
					outputTool = capturedOutput
				}
			}
		}
	}

	if opts.Progressive {
		var captureOutput func(string) error
		if outputTool != nil {
			captureOutput = func(text string) error {
				return ask.EnsureOutputToolCaptured(ctx, provider, engine.Tools(), outputToolFinalizationReq(), outputTool, text)
			}
		}
		execution, executionErr := (&askProgressiveExecution{
			ctx: ctx, stdout: deps.Stdout, stderr: deps.Stderr, flags: opts, cleanupTimeout: deps.RunnerCleanupTimeout, cfg: cfg, toolMgr: toolMgr, store: store, session: sess,
			runner: askRunner, request: baseRunReq, options: progressiveOpts, applyResult: applyRunResult,
			WrapEvents: wrapStreamEvents, useRichRenderer: useRichRenderer, jsonEmit: jsonEmit, jsonInfo: jsonInfo,
			captureOutput: captureOutput,
		}).run()
		stats, progressiveResult = execution.stats, execution.progressive
		jsonTotalTokens, jsonFinalPending = execution.jsonTotalTokens, execution.jsonFinalPending
		if executionErr != nil {
			return executionErr
		}
	} else {
		streamEvents = wrapStreamEvents(streamEvents)
		execution, executionErr := (&askStreamingExecution{
			ctx: ctx, stdout: deps.Stdout, stderr: deps.Stderr, flags: opts, cleanupTimeout: deps.RunnerCleanupTimeout, cfg: cfg, toolMgr: toolMgr, store: store, session: sess,
			events: streamEvents, adapter: adapter, runner: askRunner, request: baseRunReq,
			applyResult: applyRunResult, persistence: askPersistence, collector: collector,
			turnStart: turnStartTime, jsonEmit: jsonEmit, jsonInfo: jsonInfo, useRichRenderer: useRichRenderer,
		}).run()
		stats = execution.stats
		jsonTotalTokens = execution.jsonTotalTokens
		jsonFinalPending = execution.jsonFinalPending
		if executionErr != nil {
			return executionErr
		}
		if execution.interrupted {
			userInterrupted = true
		}
		if execution.skipFinalization {
			return nil
		}
	}

	return (&askFinalization{
		ctx: ctx, stderr: deps.Stderr, collector: collector, outputTool: outputTool, outputRequest: outputToolFinalizationReq,
		progressive: opts.Progressive, progressiveResult: progressiveResult, provider: provider, engine: engine,
		store: store, session: sess, agent: agent, json: opts.JSON, jsonEmit: jsonEmit, stats: stats,
		jsonTotalTokens: jsonTotalTokens, jsonFinalPending: jsonFinalPending, compactionUsages: &compactionUsages,
		showStats: opts.ShowStats, config: cfg,
	}).run()

}

func configureAskToolManager(ctx context.Context, stderr io.Writer, debug bool, cfg *config.Config, toolMgr *tools.ToolManager, store session.Store, sessionID string, approval resolvedApprovalMode, sessionStore appruntime.SessionStoreOverrides, paths appmemory.Paths) (*appserve.SpawnAgentRunner, error) {
	if toolMgr == nil {
		return nil, nil
	}
	toolMgr.Registry.SetPlanStore(store)
	if err := toolMgr.ConfigureWorkspacePersistence(ctx, store, sessionID); err != nil {
		return nil, err
	}
	if err := applyResolvedApprovalMode(cfg, toolMgr.ApprovalMgr, approval, approvalRuntimeOptions{}); err != nil {
		return nil, err
	}
	reportApprovalMode(stderr, debug, approval, toolMgr.ApprovalMgr)
	// PromptFunc is set in ask.StreamWithRenderer to use bubbletea UI.
	// Wire spawn_agent runner with session tracking.
	return appserve.WireSpawnAgentRunnerWithInputs(cfg, toolMgr, approval.Mode == tools.ModeYolo, store, sessionID, 0, sessionStore, paths)
}

// Restoring exact-file grants must use structured parts from both the durable
// history (including messages beyond compaction) and the active conversation.
func grantAskResumedUploadedFileReads(ctx context.Context, store session.Store, sess *session.Session, resuming bool, toolMgr *tools.ToolManager, messages []core.Message) {
	if resuming && store != nil && sess != nil {
		if rows, err := store.GetMessages(ctx, sess.ID, 0, 0); err == nil {
			history := make([]core.Message, 0, len(rows))
			for i := range rows {
				history = append(history, rows[i].ToLLMMessage())
			}
			ask.GrantUploadedFileReads(toolMgr, history)
		}
	}
	ask.GrantUploadedFileReads(toolMgr, messages)
}

func progressiveOutputText(result progressiveRunResult) string {
	return progressive.OutputText(result)
}
