package ask

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	runapproval "github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	runtimeguardian "github.com/samsaffron/term-llm/internal/app/runtime/guardian"
	"github.com/samsaffron/term-llm/internal/app/runtime/mcpstart"
	"github.com/samsaffron/term-llm/internal/app/runtime/progressive"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/mcp"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func Run(ctx context.Context, args []string, opts Options, deps Dependencies) error {
	// Extract @agent from args if present
	atAgent, filteredArgs := appruntime.ExtractAgentFromArgs(args)
	if atAgent != "" && opts.Agent == "" {
		opts.Agent = atAgent
	}

	stdinReader, hasStdin := deps.Stdin, deps.HasStdin
	question := strings.Join(filteredArgs, " ")
	progressiveOpts := progressive.AskOptions{
		Enabled:      opts.Progressive,
		Timeout:      opts.Timeout,
		StopWhen:     progressive.StopWhen(strings.TrimSpace(opts.StopWhen)),
		ContinueWith: opts.ContinueWith,
	}
	if err := progressive.ValidateAskOptions(&progressiveOpts); err != nil {
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
	if deps.ResolveApproval == nil {
		return fmt.Errorf("ask approval resolver is required")
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
	agent, err := appruntime.LoadAgent(opts.Agent, cfg)
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
	askSelected, askPolicy, err := appruntime.SelectRunModel(cfg, appruntime.ModelSelectionInput{Agent: agent, CmdProvider: cfg.Ask.Provider, CmdModel: cfg.Ask.Model, ProviderFlag: opts.Provider, Fast: opts.Fast})
	if err != nil {
		return err
	}

	bootstrap.InitThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := deps.NewProvider(cfg, opts.Fast)
	if err != nil {
		return err
	}
	engine := appruntime.NewEngine(provider, cfg)

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
		askPolicy = appruntime.ModelPolicyForSession(sess, agent)
		if sessAgent := strings.TrimSpace(sess.Agent); sessAgent != "" && (agent == nil || sessAgent != agent.Name) {
			resumedAgent, err := appruntime.LoadPersistedAgent(sessAgent, cfg)
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
	askInputCfg, err := ApplyInlineMaxBytesOverride(cfg.Ask, opts.InlineMaxBytes)
	if err != nil {
		return err
	}
	preparedInput, err := PrepareInput(question, opts.Files, stdinReader, hasStdin, askInputCfg, defaultQuestion)
	if err != nil {
		return err
	}
	question = preparedInput.Question
	keepStagedInput := false
	defer func() {
		if !keepStagedInput {
			CleanupPreparedInput(preparedInput)
		}
	}()
	settings.AutoTools = AddAutoTools(settings.AutoTools, []core.Message{preparedInput.Message})

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
	appserve.WireStaticAgentModelAdmission(cfg, toolMgr, spawnRunner, askPolicy, askSelected)
	var outputTool *tools.SetOutputTool

	if agent != nil && agent.OutputTool.IsConfigured() {
		outputTool = appruntime.RegisterAgentOutputTool(agent.OutputTool, toolMgr, engine)
	}

	appserve.RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	// Initialize MCP servers if any (after session settings are applied)
	var mcpManager *mcp.Manager
	if settings.MCP != "" {
		mcpOpts := &mcpstart.MCPOptions{
			Provider:      provider,
			YoloMode:      resolvedYolo,
			ToolDiscovery: cfg.ToolDiscovery,
		}
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			mcpOpts.Model = providerCfg.Model
		}
		mcpManager, err = EnableMCPServersWithFeedback(ctx, settings.MCP, engine, deps.Stderr, mcpOpts)
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
	var jsonEmit *JSONEmitter
	if opts.JSON {
		jsonEmit = NewJSONEmitter(deps.Stdout)
	}

	// Create stream adapter for unified event handling with proper buffering
	adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
	if sess != nil {
		adapter.Stats().SeedTotals(sess.InputTokens, sess.OutputTokens, sess.CachedInputTokens, sess.CacheWriteTokens, sess.ToolCalls, sess.LLMTurns+sess.CompactionCount)
	}
	adapter.Stats().SetModel(bootstrap.ActiveModel(cfg))
	if !opts.Progressive && toolMgr != nil {
		toolMgr.ApprovalMgr.GuardianEventFunc = func(event tools.GuardianEvent) {
			if !event.Usage.BillableCountersZero() && store != nil && sess != nil {
				appruntime.RecordStoreHelperUsage(context.Background(), store, sess.ID, event.Model, session.ModelUsageGuardian, event.Usage)
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
	var collector *TextCollector
	needsCollector := outputTool != nil || (agent != nil && (agent.Output == "commit_editmsg" || agent.OnComplete != "")) || (store != nil && sess != nil)
	wrapStreamEvents := func(events <-chan ui.StreamEvent) <-chan ui.StreamEvent {
		if !needsCollector || events == nil {
			return events
		}
		collector = &TextCollector{}
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
	persistenceCallbacks := PrepareAskPersistenceCallbacks(
		ctx, cfg, store, &sess, engine, &turnStartTime, instructions, historyHasSystem,
		resuming, messages, conversationStartedAt, preparedInput.Message, question, CompactionHooks{Record: compactionUsages.add},
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

	var jsonInfo SessionInfo
	if opts.JSON {
		agentName := ""
		if agent != nil {
			agentName = agent.Name
		}
		jsonInfo = SessionInfo{
			SessionID: sessionID,
			Provider:  provider.Name(),
			Model:     bootstrap.ActiveModel(cfg),
			Agent:     agentName,
			Tools:     settings.Tools,
			MCP:       settings.MCP,
			Yolo:      resolvedYolo,
			Search:    settings.Search,
			Resuming:  resuming,
		}
	}

	var stats *ui.SessionStats
	var progressiveResult progressive.RunResult
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
		Model:                       bootstrap.ActiveModel(cfg),
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
				return EnsureOutputToolCaptured(ctx, provider, engine.Tools(), outputToolFinalizationReq(), outputTool, text)
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

func configureAskToolManager(ctx context.Context, stderr io.Writer, debug bool, cfg *config.Config, toolMgr *tools.ToolManager, store session.Store, sessionID string, approval runapproval.ResolvedMode, sessionStore appruntime.SessionStoreOverrides, paths appmemory.Paths) (*appserve.SpawnAgentRunner, error) {
	if toolMgr == nil {
		return nil, nil
	}
	toolMgr.Registry.SetPlanStore(store)
	if err := toolMgr.ConfigureWorkspacePersistence(ctx, store, sessionID); err != nil {
		return nil, err
	}
	if err := runtimeguardian.ApplyResolvedMode(cfg, toolMgr.ApprovalMgr, approval, runapproval.RuntimeOptions{}); err != nil {
		return nil, err
	}
	runapproval.Report(stderr, debug, approval, toolMgr.ApprovalMgr)
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
			GrantUploadedFileReads(toolMgr, history)
		}
	}
	GrantUploadedFileReads(toolMgr, messages)
}

func progressiveOutputText(result progressive.RunResult) string {
	return progressive.OutputText(result)
}
