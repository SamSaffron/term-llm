package chat

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	runapproval "github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	runtimeguardian "github.com/samsaffron/term-llm/internal/app/runtime/guardian"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

// chatSessionLaunch describes one session runtime to construct: the initial
// launch, a quit-and-relaunch iteration, or an in-process session switch.
type chatSessionLaunch struct {
	initialText      string
	cliAgent         string
	resumeRequested  bool
	resumeID         string
	handoverAutoSend string
	relaunchHandoff  *chatRelaunchHandoff
	config           *config.Config
	warningSink      io.Writer
}

// chatSessionRuntime owns every per-session resource behind a visible chat
// model. Exactly one runtime is wired to the Bubble Tea program at a time; an
// in-process session switch builds a replacement runtime and disposes (or
// transfers to the MainRunManager) the previous one without restarting the
// program, so the terminal never leaves the alternate screen.
type chatSessionRuntime struct {
	model         *chat.Model
	store         session.Store
	storeCleanup  func()
	storeWarnings *TUIWarningWriter
	sess          *session.Session
	toolMgr       *tools.ToolManager
	approvalMgr   *tools.ApprovalManager
	spawnRunner   *appserve.SpawnAgentRunner
	useAltScreen  bool
	// cleanupResources is once-guarded: drains sub-agents, stops MCP, closes
	// Guardian and the debug logger, then closes the session store.
	cleanupResources func()
	// restoreTitle resets the terminal title once for this runtime's model.
	restoreTitle func()
}

func (r *chatInvocation) buildChatSessionRuntime(ctx context.Context, launch chatSessionLaunch, mainRuns *chat.MainRunManager) (*chatSessionRuntime, error) {
	initialText := launch.initialText
	cliAgent := launch.cliAgent
	resumeRequested := launch.resumeRequested
	resumeID := launch.resumeID
	handoverAutoSend := launch.handoverAutoSend
	relaunchHandoff := launch.relaunchHandoff

	cfg := launch.config
	var err error
	if cfg == nil {
		cfg, err = r.deps.LoadConfig()
		if err != nil {
			return nil, err
		}
	}
	// The initial runtime shares the exact launch snapshot used to construct the
	// terminal-host manager. Session switches and relaunches pass nil and reload.
	rawConfigInstructions := cfg.Chat.Instructions

	// Initialize session store EARLY so resume can override settings before tool/MCP setup.
	runtimeSessionStore, runtimeMemoryPaths := r.deps.SessionStore, r.deps.MemoryPaths
	// Store warnings are raised in the background for the whole TUI lifetime, so
	// they route through the program instead of stderr once it is rendering.
	storeWarnings, finishWarningsBuild := NewSessionWarningWriter(r.deps.Stderr, launch.warningSink)
	defer finishWarningsBuild()
	warnings := storeWarnings
	store, storeCleanup := runtime.InitSessionStoreWith(cfg, storeWarnings, runtimeSessionStore)
	var spawnRunner *appserve.SpawnAgentRunner
	// Failure path: release everything constructed so far, newest first. The
	// success path hands cleanup ownership to the returned runtime.
	built := false
	var failureCleanup []func()
	defer func() {
		if built {
			return
		}
		for i := len(failureCleanup) - 1; i >= 0; i-- {
			failureCleanup[i]()
		}
	}()
	failureCleanup = append(failureCleanup, storeCleanup)

	var sess *session.Session
	if resumeRequested {
		if store == nil {
			return nil, fmt.Errorf("session storage is disabled; cannot resume")
		}
		sess, err = resolveChatResumeSession(context.Background(), store, resumeID)
		if err != nil {
			return nil, err
		}
		if err := store.SetCurrent(context.Background(), sess.ID); err != nil {
			return nil, fmt.Errorf("select resumed session: %w", err)
		}
		// Normalize persisted directory metadata before resolving any prompt,
		// project instructions, or skills. A missing worktree falls back to the
		// root/process directory through the same path used for tool binding.
		if err := appserve.RestoreWorktreeBinding(context.Background(), store, sess, nil); err != nil {
			fmt.Fprintf(warnings, "warning: failed to restore session directory: %v\n", err)
		}
	}
	runtimeDir := runtime.EffectiveSessionDirectory(sess)

	// Saved session agent wins on resume.
	effectiveAgent := strings.TrimSpace(cliAgent)
	if sess != nil {
		effectiveAgent = strings.TrimSpace(sess.Agent)
	}

	agent, err := runtime.LoadAgent(effectiveAgent, cfg)
	if err != nil {
		return nil, err
	}

	var inputTicket *appserve.SessionInputTicket
	var selectedInputs *appserve.SessionInputSelection
	if sess != nil {
		if _, supported := session.AsSessionInputRefresher(store); supported {
			inputTicket, err = appserve.ProcessSessionInputs().Acquire(ctx, store, sess.ID, appserve.InputBinding(effectiveAgent, runtimeDir))
			if err != nil {
				return nil, err
			}
			defer inputTicket.Fail()
			selectedInputs = inputTicket.Selected()
		}
	}
	// Resolve all settings: CLI > agent > config (resume overrides applied below).
	settings, err := appserve.ResolveSettingsInDir(cfg, agent, appserve.CLIFlags{
		Provider:         r.opts.Provider,
		Inputs:           selectedInputs,
		ToolsSet:         r.opts.Changed.Tools,
		SystemMessageSet: r.opts.Changed.System,
		Tools:            r.opts.Tools,
		ReadDirs:         r.opts.ReadDirs,
		WriteDirs:        r.opts.WriteDirs,
		ShellAllow:       r.opts.ShellAllow,
		MCP:              r.opts.MCP,
		SystemMessage:    r.opts.SystemMessage,
		MaxTurns:         r.opts.MaxTurns,
		MaxTurnsSet:      r.opts.Changed.MaxTurns,
		Search:           r.opts.Search,
		NoSearch:         r.opts.NoSearch,
		Platform:         "chat",
		MemoryPaths:      runtimeMemoryPaths,
	}, cfg.Chat.Provider, cfg.Chat.Model, rawConfigInstructions, cfg.Chat.MaxTurns, 200, runtimeDir)
	if err != nil {
		return nil, err
	}
	settings.PrimaryWorkspace = runtimeDir
	applyChatSearchDefault(&settings, r.opts.NoSearch, sess, agent != nil)

	// Saved session settings win on resume.
	if sess != nil {
		settings.Search = runtime.ResumedSessionSearch(settings.Search, sess.Search, r.opts.Changed.Search, r.opts.NoSearch)
		if inputTicket == nil {
			settings.Tools = sess.Tools
		}
		settings.MCP = sess.MCP
		settings.SessionID = sess.ID
	}

	// Resolve requested approval policy before runtime guardian setup so a
	// temporary interactive fallback does not change what the session persists.
	var persistedApprovalMode *session.SessionApprovalMode
	if sess != nil {
		persistedApprovalMode = &sess.ApprovalMode
	}
	if r.deps.ResolveApproval == nil {
		return nil, fmt.Errorf("chat approval resolver is required")
	}
	resolvedApproval, err := r.deps.ResolveApproval(cfg, persistedApprovalMode, r.handoverApproval)
	if err != nil {
		return nil, err
	}
	desiredApprovalMode := resolvedApproval.Mode
	resolvedYolo := desiredApprovalMode == tools.ModeYolo
	r.opts.Approval = desiredApprovalMode.String()
	r.opts.Yolo = resolvedYolo
	r.opts.AutoApproval = desiredApprovalMode == tools.ModeAuto

	if sess != nil {
		resumeProvider := runtime.ResolveSessionProviderKey(cfg, sess)
		if resumeProvider == "" {
			resumeProvider = cfg.DefaultProvider
			fmt.Fprintf(warnings, "warning: unable to infer provider for session %s; falling back to %s\n", session.ShortID(sess.ID), resumeProvider)
		}
		providerOverride := resumeProvider
		if model := strings.TrimSpace(sess.Model); model != "" {
			providerOverride = resumeProvider + ":" + model
		}
		if _, _, err := runtime.SelectRunModel(cfg, runtime.ModelSelectionInput{Agent: agent, CmdProvider: cfg.Chat.Provider, CmdModel: cfg.Chat.Model, ProviderFlag: providerOverride, Inherited: sess.ModelPolicy}); err != nil {
			return nil, err
		}
	} else {
		if _, _, err := runtime.SelectRunModel(cfg, runtime.ModelSelectionInput{Agent: agent, CmdProvider: cfg.Chat.Provider, CmdModel: cfg.Chat.Model, ProviderFlag: r.opts.Provider}); err != nil {
			return nil, err
		}
	}

	bootstrap.InitThemeFromConfig(cfg)

	titleMode, titleModeOK := chat.ParseTerminalTitleMode(cfg.Chat.TerminalTitle)
	if !titleModeOK {
		fmt.Fprintf(warnings, "warning: unknown chat.terminal_title %q; using %q\n", cfg.Chat.TerminalTitle, titleMode)
	}
	cfg.Chat.TerminalTitle = string(titleMode)
	if err := chat.ValidateTerminalTitleFormat(cfg.Chat.TerminalTitleFormat); err != nil {
		fmt.Fprintf(warnings, "warning: invalid chat.terminal_title_format %q: %v; using default title format\n", cfg.Chat.TerminalTitleFormat, err)
		cfg.Chat.TerminalTitleFormat = ""
	}

	// Create LLM provider and engine
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return nil, err
	}
	effectivePolicy := runtime.ModelPolicyForSession(sess, agent)
	fastProvider, fastErr := runtime.AuxProviderForPolicy(cfg, effectivePolicy, runtime.ModelSelection{Provider: cfg.DefaultProvider, Model: bootstrap.ActiveModel(cfg)}, cfg.DefaultProvider)
	if fastErr != nil {
		fmt.Fprintf(warnings, "warning: fast provider setup failed: %v\n", fastErr)
	}
	engine := runtime.NewEngine(provider, cfg)

	// Set up debug logger if enabled.
	// We close the logger manually after MCP cleanup (not via defer) because
	// MCP servers may still log during shutdown, and the TUI blocks until exit.
	debugLogger, debugLoggerErr := r.deps.CreateDebugLogger(cfg)
	if debugLoggerErr != nil {
		fmt.Fprintf(warnings, "warning: %v\n", debugLoggerErr)
	}
	if debugLogger != nil {
		engine.SetDebugLogger(debugLogger)
	}
	failureCleanup = append(failureCleanup, func() {
		if debugLogger != nil {
			debugLogger.Close()
		}
	})

	// Initialize tools if enabled (using possibly-updated settings from resume)
	appserve.AlignSettingsToActiveProvider(&settings, cfg, provider)
	enabledLocalTools := tools.ParseToolsFlag(settings.Tools)
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		return nil, err
	}
	if toolMgr != nil {
		toolMgr.Registry.SetPlanStore(store)
		workspaceSessionID := ""
		if sess != nil {
			workspaceSessionID = sess.ID
		}
		if err := toolMgr.ConfigureWorkspacePersistence(context.Background(), store, workspaceSessionID); err != nil {
			return nil, err
		}
	}
	if sess != nil && toolMgr != nil {
		if err := appserve.RestoreWorktreeBinding(context.Background(), store, sess, toolMgr); err != nil {
			fmt.Fprintf(warnings, "warning: failed to restore worktree binding: %v\n", err)
		}
	}
	approvalMgr, err := buildChatHandoverApprovalManager(cfg, settings)
	if err != nil {
		return nil, err
	}
	// Error-path safety net. The normal exit path closes Guardian explicitly
	// before the debug logger so provider cleanup can still emit diagnostics.
	// The closure reads approvalMgr so the toolMgr reassignment below is seen.
	failureCleanup = append(failureCleanup, func() {
		if approvalMgr != nil {
			approvalMgr.Close()
		}
	})
	if toolMgr != nil {
		approvalMgr = toolMgr.ApprovalMgr
		if err := runtimeguardian.ApplyResolvedMode(cfg, approvalMgr, resolvedApproval, runapproval.RuntimeOptions{
			PrepareCallbacks: true,
			WarningWriter:    storeWarnings,
		}); err != nil {
			return nil, err
		}

		// output_tool defines a single-shot return channel for ask. Chat has no
		// single final output contract, so do not register it as an interactive
		// finishing tool.

		// PromptUIFunc will be set up below after tea.Program is created

		// Wire spawn_agent runner if enabled (with session tracking)
		var parentSessionID string
		if sess != nil {
			parentSessionID = sess.ID
		}
		var wireErr error
		spawnRunner, wireErr = appserve.WireSpawnAgentRunnerWithInputs(cfg, toolMgr, resolvedYolo, store, parentSessionID, 0, runtimeSessionStore, runtimeMemoryPaths)
		if wireErr != nil {
			return nil, wireErr
		}
	} else {
		if err := runtimeguardian.ApplyResolvedMode(cfg, approvalMgr, resolvedApproval, runapproval.RuntimeOptions{
			PrepareCallbacks: true,
			WarningWriter:    storeWarnings,
		}); err != nil {
			return nil, err
		}
	}
	runapproval.Report(warnings, r.opts.Debug, resolvedApproval, approvalMgr)

	// Initialize skills system
	agentSkills := ""
	if agent != nil {
		agentSkills = agent.Skills
	}
	skillsSetup := appserve.SetupSkillsInDir(&cfg.Skills, r.opts.Skills, agentSkills, warnings, runtimeDir)

	// Store resolved instructions in config for chat TUI
	cfg.Chat.Instructions = appserve.InjectSkillsMetadata(settings.SystemPrompt, skillsSetup)
	if selectedInputs != nil {
		cfg.Chat.Instructions = selectedInputs.Prompt
	}

	appserve.RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	// Direct isolated skills use the generic child runner even when the current
	// agent does not expose the model-facing spawn_agent tool.
	if skillsSetup != nil && spawnRunner == nil {
		parentSessionID := ""
		if sess != nil {
			parentSessionID = sess.ID
		}
		spawnRunner, err = appserve.NewSpawnAgentRunnerWithStore(cfg, resolvedYolo, approvalMgr, store, parentSessionID)
		if err != nil {
			return nil, fmt.Errorf("initialize isolated skill runner: %w", err)
		}
		spawnRunner.SetRuntimeInputs(runtimeSessionStore, runtimeMemoryPaths)
		spawnRunner.SetBaseDirFunc(func() string {
			if toolMgr != nil {
				if dir := strings.TrimSpace(toolMgr.BaseDir()); dir != "" {
					return dir
				}
			}
			if sess != nil {
				if dir := strings.TrimSpace(sess.WorktreeDir); dir != "" {
					return dir
				}
				if dir := strings.TrimSpace(sess.CWD); dir != "" {
					return dir
				}
			}
			return runtimeDir
		})
	}

	// Determine model name
	modelName := getModelName(cfg)
	if modelName == "" {
		modelName = runtime.ExtractModelFromProviderName(provider.Name())
	}
	providerKey := cfg.DefaultProvider

	if inputTicket != nil {
		if inputTicket.Owner() {
			result, refreshErr := func() (session.SessionInputRefreshResult, error) {
				refresher, _ := session.AsSessionInputRefresher(store)
				return refresher.RefreshSessionInputs(ctx, sess.ID, cfg.Chat.Instructions, settings.Tools, appserve.EqualSessionTools)
			}()
			if refreshErr != nil {
				return nil, refreshErr
			}
			settings.Tools = result.Tools
		}
		sess.Tools = settings.Tools
	}
	// Normalize resumed session metadata to canonical provider key + active model.
	agentName := ""
	if agent != nil {
		agentName = agent.Name
	}
	if sess != nil {
		sess.Provider = provider.Name()
		sess.ProviderKey = providerKey
		sess.Model = modelName
		sess.Agent = agentName
		sess.ApprovalMode = runapproval.ToSession(desiredApprovalMode)
		_ = store.Update(context.Background(), sess)
	}

	// Create MCP manager
	mcpManager := mcp.NewManager()
	failureCleanup = append(failureCleanup, mcpManager.StopAll)
	if err := mcpManager.LoadConfig(); err != nil {
		// Non-fatal: continue without MCP
		fmt.Fprintf(warnings, "Warning: failed to load MCP config: %v\n", err)
	}

	configureChatMCPServers(ctx, mcpManager, provider, modelName, resolvedYolo, settings.MCP, warnings)

	// Resolve force external search setting
	forceExternalSearch := bootstrap.ResolveForceExternalSearch(cfg, r.opts.NativeSearch, r.opts.NoNativeSearch)

	// Only enable alt-screen when stdout is a terminal (avoid corrupting piped output)
	// Disable alt-screen in auto-send mode for clean output
	autoSendMode := len(r.opts.AutoSend) > 0
	useAltScreen := r.deps.StdoutIsTerminal() && !autoSendMode

	// Create chat model
	chatPlatformMessage := ""
	if agent != nil {
		chatPlatformMessage = agent.PlatformMessages.For("chat")
	}
	// The /model picker lists configured providers only; probe local logins in
	// the background so opening it never waits on them.
	llm.DefaultProviderCredentials.Warm(cfg)
	model := chat.NewWithFastProviderAndApproval(cfg, provider, fastProvider, engine, providerKey, modelName, mcpManager, settings.MaxTurns, forceExternalSearch, r.opts.NoWebFetch, settings.Search, enabledLocalTools, settings.Tools, settings.MCP, false, initialText, store, sess, useAltScreen, r.opts.AutoSend, autoSendMode, r.opts.TextMode, agentName, chatPlatformMessage, resolvedYolo, desiredApprovalMode, toolMgr)
	model.SetModelPolicy(effectivePolicy)
	var liveModelMu sync.RWMutex
	liveModel := runpkg.ParentModel{Provider: providerKey, Model: modelName}
	liveState := func() (modelpolicy.Policy, runpkg.ParentModel) {
		liveModelMu.RLock()
		defer liveModelMu.RUnlock()
		return effectivePolicy, liveModel
	}
	if spawnRunner != nil {
		spawnRunner.SetModelPolicySource(func() appserve.ParentModelState {
			p, parent := liveState()
			return appserve.ParentModelState{Policy: p, Provider: parent.Provider, Model: parent.Model}
		})
	}
	if toolMgr != nil && toolMgr.Registry != nil {
		toolMgr.Registry.SetAgentModelAdmission(liveState, func(ctx context.Context, name, override string, inherited modelpolicy.Policy, parent runpkg.ParentModel) error {
			return appserve.NewRunner(cfg, appserve.RunnerOptions{}).CheckAgentModel(ctx, runpkg.Request{AgentName: name, Model: override, ModelPolicy: inherited, ParentModel: parent})
		})
	}
	model.SetModelSwitchHook(func(key, name string) {
		liveModelMu.Lock()
		liveModel = runpkg.ParentModel{Provider: key, Model: name}
		liveModelMu.Unlock()
	})
	model.SetFastProviderResolver(func(key, name string) (core.Provider, error) {
		return runtime.AuxProviderForPolicy(cfg, effectivePolicy, runtime.ModelSelection{Provider: key, Model: name}, key, cfg.DefaultProvider)
	})
	model.SetAgentMentionCapability(RuntimeAgentMentionCapability{Engine: model.CurrentAgentMentionEngine, Manager: toolMgr})
	if sess != nil {
		model.SetConversationBranch(sessionIsConversationBranch(context.Background(), store, sess.ID))
	}
	model.ConfigureTerminalTitleEnvironment(chat.TerminalTitleEnvironmentFromEnv())
	terminalTitleRestored := false
	restoreTerminalTitle := func() {
		if terminalTitleRestored {
			return
		}
		terminalTitleRestored = true
		model.RestoreTerminalTitle()
	}
	if agent != nil && agent.OutputTool.IsConfigured() {
		model.SetFooterWarning("agent output_tool is ignored in chat; use ask for tool-captured output")
	}
	model.SetRootContext(ctx)
	agentOwner := &runtime.AgentHostOwner{}
	model.SetRunner(appserve.NewCmdRunner(cfg, appserve.RunnerOptions{
		AgentOwner:         agentOwner,
		Provider:           r.opts.Provider,
		ConfigSet:          true,
		ConfigProvider:     cfg.Chat.Provider,
		ConfigModel:        cfg.Chat.Model,
		ConfigInstructions: cfg.Chat.Instructions,
		ConfigMaxTurns:     cfg.Chat.MaxTurns,
		Tools:              settings.Tools,
		ReadDirs:           append([]string(nil), r.opts.ReadDirs...),
		WriteDirs:          append([]string(nil), r.opts.WriteDirs...),
		ShellAllow:         append([]string(nil), r.opts.ShellAllow...),
		MCP:                settings.MCP,
		MaxTurns:           settings.MaxTurns,
		DefaultMaxTurns:    200,
		Search:             settings.Search,
		NoSearch:           r.opts.NoSearch,
		NativeSearch:       r.opts.NativeSearch,
		NoNativeSearch:     r.opts.NoNativeSearch,
		ApprovalMode:       resolvedApproval.Mode,
		ApprovalModeSet:    true,
		ApprovalSource:     resolvedApproval.Source,
		Debug:              r.opts.Debug,
		DebugRaw:           r.opts.DebugRaw,
		ErrWriter:          ChildProgressWriter(),
		Store:              store,
		SessionStore:       runtimeSessionStore,
		MemoryPaths:        runtimeMemoryPaths,
		ParentApprovalMgr:  approvalMgr,
	}))
	model.SetChildRunner(spawnRunner)
	model.SetCommitMutationCoordinator(appserve.CommitRootCoordinator{})
	model.SetMainRunManager(mainRuns)

	// Wire handover auto-send if pending from previous iteration
	if handoverAutoSend != "" {
		model.SetHandoverAutoSend(handoverAutoSend)
	}
	if relaunchHandoff != nil {
		if relaunchHandoff.branchPrefill != "" {
			model.SetBranchPrefill(relaunchHandoff.branchPrefill)
			relaunchHandoff.branchPrefill = ""
		}
		if relaunchHandoff.branchPathNotes != nil {
			model.SetBranchPathNotes(relaunchHandoff.branchPathNotes)
			relaunchHandoff.branchPathNotes = nil
		}
		if relaunchHandoff.branchAutoSend != "" {
			model.SetBranchAutoSend(relaunchHandoff.branchAutoSend)
			relaunchHandoff.branchAutoSend = ""
		}
	}
	model.SetSideQuestionProviderFactory(func(providerKey, modelName string) (core.Provider, error) {
		if strings.TrimSpace(providerKey) == "" {
			providerKey = provider.Name()
		}
		if err := effectivePolicy.CheckWithConfig(cfg, agentName, providerKey, modelName); err != nil {
			return nil, err
		}
		return llm.NewProviderByName(cfg, providerKey, modelName)
	})
	model.SetHandoverApprovalManager(approvalMgr)
	model.PersistApprovalMode(desiredApprovalMode)

	// Wire agent resolver, lister, and current agent for /handover support
	model.SetAgentResolver(runtime.LoadAgent)
	currentRuntimeContext := chat.RuntimeSystemContext{
		SystemPrompt: cfg.Chat.Instructions,
		ApplySkills:  skillContextApplier(skillsSetup),
		Skills:       skillsSetup,
	}
	model.SetRuntimeSystemContextResolver(func(targetAgent *agents.Agent, providerKey, modelName, dir string) (chat.RuntimeSystemContext, error) {
		systemMessage := r.opts.SystemMessage
		if targetAgent != agent {
			systemMessage = ""
		}
		return r.resolveChatRuntimeSystemContextWithConfig(cfg, targetAgent, providerKey, modelName, dir, rawConfigInstructions, systemMessage, runtimeMemoryPaths)
	}, currentRuntimeContext)
	model.SetHandoverSystemPromptResolver(func(targetAgent *agents.Agent, providerKey, modelName string) (string, error) {
		return r.resolveChatHandoverSystemPrompt(targetAgent, providerKey, modelName, runtimeMemoryPaths)
	})
	model.SetAgentLister(runtime.ListAgentNames)
	if agent != nil {
		model.SetCurrentAgent(agent)
	}

	var cleanupOnce sync.Once
	cleanupResources := func() {
		cleanupOnce.Do(func() {
			if spawnRunner != nil {
				appserve.ShutdownSpawnAgentRunner(spawnRunner)
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = agentOwner.Shutdown(shutdownCtx)
			cancel()
			mcpManager.StopAll()
			if approvalMgr != nil {
				approvalMgr.Close()
			}
			if debugLogger != nil {
				debugLogger.Close()
			}
			storeCleanup()
		})
	}

	built = true
	if inputTicket != nil && inputTicket.Owner() {
		inputTicket.Finish(appserve.SessionInputSelection{BasePrompt: settings.SystemPrompt, Prompt: cfg.Chat.Instructions, Tools: settings.Tools})
	}
	if _, supported := session.AsSessionInputRefresher(store); supported {
		model.SetSessionInputsObserver(func(bound *session.Session, prompt, toolsSetting string) {
			appserve.RegisterOwnedSessionInputs(store, bound, appserve.SessionInputSelection{BasePrompt: prompt, Prompt: prompt, Tools: toolsSetting})
		})
	}
	return &chatSessionRuntime{
		model:            model,
		store:            store,
		storeCleanup:     storeCleanup,
		storeWarnings:    storeWarnings,
		sess:             sess,
		toolMgr:          toolMgr,
		approvalMgr:      approvalMgr,
		spawnRunner:      spawnRunner,
		useAltScreen:     useAltScreen,
		cleanupResources: cleanupResources,
		restoreTitle:     restoreTerminalTitle,
	}, nil
}
