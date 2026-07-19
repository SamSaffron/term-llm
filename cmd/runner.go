package cmd

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type cmdRunnerOptions struct {
	Provider           string
	Fast               bool
	ConfigSet          bool
	ConfigProvider     string
	ConfigModel        string
	ConfigInstructions string
	ConfigMaxTurns     int
	Tools              string
	ReadDirs           []string
	WriteDirs          []string
	ShellAllow         []string
	MCP                string
	SystemMessage      string
	MaxTurns           int
	DefaultMaxTurns    int
	Search             bool
	NoSearch           bool

	NativeSearch   bool
	NoNativeSearch bool

	Yolo     bool
	Auto     bool
	Debug    bool
	DebugRaw bool

	ErrWriter         io.Writer
	WireSpawn         func(*config.Config, *tools.ToolManager, bool) error
	Store             session.Store
	ParentApprovalMgr *tools.ApprovalManager
}

type cmdRunner struct {
	baseCfg  *config.Config
	defaults cmdRunnerOptions
}

func newRunner(cfg *config.Config) runpkg.Runner {
	return newCmdRunner(cfg, cmdRunnerOptions{})
}

func newCmdRunner(cfg *config.Config, opts cmdRunnerOptions) runpkg.Runner {
	if opts.ErrWriter == nil {
		opts.ErrWriter = io.Discard
	}
	return &cmdRunner{baseCfg: cfg, defaults: opts}
}

type cmdRunEnvironment struct {
	cfg           *config.Config
	req           runpkg.Request
	runtime       *serveRuntime
	engine        *llm.Engine
	provider      llm.Provider
	modelName     string
	settings      SessionSettings
	store         session.Store
	closeStore    func()
	sess          *session.Session
	llmReq        llm.Request
	inputMessages []llm.Message
}

func (env *cmdRunEnvironment) Close() {
	if env == nil {
		return
	}
	if env.runtime != nil {
		env.runtime.Close()
	}
	if env.closeStore != nil {
		env.closeStore()
	}
}

func (r *cmdRunner) Run(ctx context.Context, req runpkg.Request, sink runpkg.EventSink) (runpkg.Result, error) {
	env, err := r.prepare(ctx, req, sink)
	if err != nil {
		return runpkg.Result{}, err
	}
	defer env.Close()
	if env.req.OnEngineReady != nil {
		env.req.OnEngineReady(env.engine)
	}
	if env.req.OnEngineDone != nil {
		defer env.req.OnEngineDone(env.engine)
	}

	collector := &runnerEventCollector{sink: sink}
	if env.req.Progressive != nil {
		return r.runProgressive(ctx, env.runtime, env.engine, env.llmReq, env.inputMessages, env.req, env.sess, env.store, env.provider, collector)
	}

	serveResult, err := env.runtime.RunWithEvents(ctx, env.req.Stateful, env.req.ReplaceHistory, env.inputMessages, env.llmReq, func(ev llm.Event) error {
		return collector.Event(ev)
	})
	result := collector.Result(env.req.SessionID)
	result.Provider = env.provider.Name()
	result.Model = env.modelName
	result.Engine = env.engine
	result.ProviderInstance = env.provider
	if result.Response == "" {
		result.Response = serveResult.Text.String()
	}
	return result, err
}

func (r *cmdRunner) prepare(ctx context.Context, req runpkg.Request, sink runpkg.EventSink) (*cmdRunEnvironment, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Platform == "" {
		req.Platform = runpkg.PlatformConsole
	}
	cfg := cloneConfigForServeJob(r.baseCfg)

	agent, err := LoadAgent(req.AgentName, cfg)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.AgentName) != "" && agent == nil {
		return nil, fmt.Errorf("agent %q not found", req.AgentName)
	}

	agentProvider, agentModel, agentSkills, agentName := "", "", "", ""
	if agent != nil {
		if model := strings.TrimSpace(req.Model); model != "" {
			agentCopy := *agent
			agentCopy.Model = model
			agent = &agentCopy
		}
		agentProvider = agent.Provider
		agentModel = agent.Model
		agentSkills = agent.Skills
		agentName = agent.Name
	}
	providerFlag := strings.TrimSpace(req.Provider)
	if providerFlag == "" {
		providerFlag = strings.TrimSpace(r.defaults.Provider)
	}
	cmdProvider, cmdModel, _, _ := r.commandConfig(cfg)
	if err := applyProviderOverridesWithAgent(cfg, cmdProvider, cmdModel, providerFlag, agentProvider, agentModel); err != nil {
		return nil, err
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		if err := applyAgentModelOverride(cfg, model); err != nil {
			return nil, fmt.Errorf("apply model override %q: %w", model, err)
		}
	}

	if strings.TrimSpace(req.SessionID) == "" && !req.DeferSession {
		req.SessionID = session.NewID()
	}

	settings, err := r.resolveSettings(cfg, agent, req, providerFlag)
	if err != nil {
		return nil, err
	}

	settings.SessionID = req.SessionID
	skillsSetup := SetupSkillsInDir(&cfg.Skills, req.Skills, agentSkills, r.errWriter(), settings.BaseDir)
	settings.SystemPrompt = InjectSkillsMetadata(settings.SystemPrompt, skillsSetup)
	settings.SystemPrompt = appendChildSkillSystemContext(settings.SystemPrompt, req.ChildSkill)

	modelName := activeModel(cfg)
	provider := req.ProviderInstance
	providerOwned := provider == nil
	if provider == nil {
		if r.defaults.Fast {
			provider, err = llm.NewFastProvider(cfg, cfg.DefaultProvider)
			if err != nil {
				return nil, fmt.Errorf("fast provider: %w", err)
			}
			if provider == nil {
				return nil, fmt.Errorf("no fast provider configured for %q", cfg.DefaultProvider)
			}
		} else {
			provider, err = llm.NewProvider(cfg)
			if err != nil {
				return nil, err
			}
		}
	}
	alignSettingsToActiveProvider(&settings, cfg, provider)

	var store session.Store
	var closeStore func()
	if r.defaults.Store != nil {
		store = r.defaults.Store
	} else if req.Persist {
		store, closeStore = InitSessionStore(cfg, r.errWriter())
	}
	var runtime *serveRuntime
	cleanupOnError := true
	defer func() {
		if !cleanupOnError {
			return
		}
		if runtime != nil {
			runtime.Close()
		}
		if closeStore != nil {
			closeStore()
		}
	}()

	yoloMode := r.defaults.Yolo || req.Yolo
	autoMode := r.defaults.Auto || req.Auto
	toolYoloMode := yoloMode
	if r.defaults.ParentApprovalMgr != nil {
		// Sub-agents inherit approval/yolo state dynamically from the parent manager.
		toolYoloMode = false
	}
	wireSpawn := r.defaults.WireSpawn
	if wireSpawn == nil {
		wireSpawn = func(cfg *config.Config, toolMgr *tools.ToolManager, _ bool) error {
			_, err := WireSpawnAgentRunnerWithStoreAndDepth(cfg, toolMgr, yoloMode, store, req.SessionID, req.Depth)
			return err
		}
	}
	engine := req.Engine
	var toolMgr *tools.ToolManager
	borrowedEngine := engine != nil
	if borrowedEngine {
		engine.ConfigureContextManagement(provider, cfg.DefaultProvider, modelName, cfg.AutoCompact)
	} else {
		engine, toolMgr, err = newServeEngineWithTools(cfg, settings, provider, cfg.DefaultProvider, modelName, toolYoloMode, autoMode, wireSpawn, skillsSetup)
		if err != nil {
			return nil, err
		}
		if agent != nil && agent.OutputTool.IsConfigured() && req.Platform != runpkg.PlatformChat {
			registerAgentOutputTool(agent.OutputTool, toolMgr, engine)
		}
	}
	if err := applyChildSkillRuntime(engine, toolMgr, req.ChildSkill); err != nil {
		return nil, err
	}
	if req.ContextEstimateTotalTokens > 0 {
		engine.SetContextEstimateBaseline(req.ContextEstimateTotalTokens, req.ContextEstimateMessageCount)
	}
	if toolMgr != nil && r.defaults.ParentApprovalMgr != nil {
		if err := toolMgr.ApprovalMgr.SetParent(r.defaults.ParentApprovalMgr); err != nil {
			return nil, fmt.Errorf("set parent approval manager: %w", err)
		}
	}
	configureInteractiveSink(toolMgr, sink)

	forceExternalSearch := resolveForceExternalSearch(cfg, r.defaults.NativeSearch, r.defaults.NoNativeSearch)
	if req.ForceExternalSearch != nil {
		forceExternalSearch = *req.ForceExternalSearch
	}

	runtimeStore := store
	if req.DisableRuntimePersistence {
		runtimeStore = nil
	}
	runtime = &serveRuntime{
		provider:            provider,
		providerKey:         cfg.DefaultProvider,
		engine:              engine,
		toolMgr:             toolMgr,
		store:               runtimeStore,
		goalStore:           store,
		systemPrompt:        settings.SystemPrompt,
		search:              settings.Search,
		forceExternalSearch: forceExternalSearch,
		maxTurns:            settings.MaxTurns,
		debug:               r.defaults.Debug || req.Debug,
		debugRaw:            r.defaults.DebugRaw || req.DebugRaw,
		autoCompact:         cfg.AutoCompact,
		skipProviderCleanup: !providerOwned,
		defaultModel:        modelName,
		yoloMode:            yoloMode,
		toolsSetting:        settings.Tools,
		mcpSetting:          settings.MCP,
		agentName:           agentName,
		platform:            templatePlatform(req.Platform),
	}
	if askUser, ok := sink.(runpkg.AskUserPrompter); ok {
		runtime.askUserFunc = askUser.AskUser
	}
	runtime.assistantSnapshotCB = req.OnAssistantSnapshot
	runtime.responseCompletedCB = req.OnResponseCompleted
	runtime.turnCompletedCB = req.OnTurnCompleted
	runtime.compactionCB = req.OnCompaction
	runtime.syntheticUserCB = req.OnSyntheticUserMessage

	var sess *session.Session
	if store != nil && !req.DeferSession {
		sess = r.ensureRunSession(ctx, store, req, provider, cfg.DefaultProvider, modelName, agentName, settings)
		runtime.sessionMeta = sess
	}

	if settings.MCP != "" && !borrowedEngine {
		mcpOpts := &MCPOptions{Provider: provider, Model: modelName, YoloMode: yoloMode}
		mgr, err := enableMCPServersWithFeedback(ctx, settings.MCP, engine, r.errWriter(), mcpOpts)
		if err != nil {
			return nil, err
		}
		runtime.mcpManager = mgr
	}

	configuredTools := true
	if req.IncludeConfiguredTools != nil {
		configuredTools = *req.IncludeConfiguredTools
	}
	var toolSpecs []llm.ToolSpec
	if configuredTools {
		toolSpecs = runtime.selectTools(nil)
	}
	if len(req.ExtraTools) > 0 {
		toolSpecs = append(toolSpecs, req.ExtraTools...)
	}
	toolSpecs = filterEngineAllowedToolSpecs(toolSpecs, engine)
	toolChoice := llm.ToolChoice{}
	if len(toolSpecs) > 0 {
		toolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}
	caps := provider.Capabilities()
	if req.ForceToolName != "" && caps.SupportsToolChoice {
		toolChoice = llm.ToolChoice{Mode: llm.ToolChoiceName, Name: req.ForceToolName}
	}
	var lastTurnToolChoice *llm.ToolChoice
	if req.LastTurnForceToolName != "" && caps.SupportsToolChoice {
		choice := llm.ToolChoice{Mode: llm.ToolChoiceName, Name: req.LastTurnForceToolName}
		lastTurnToolChoice = &choice
	}

	llmReq := llm.Request{
		Model:                    modelName,
		SessionID:                req.SessionID,
		WorkingDir:               settings.BaseDir,
		Tools:                    toolSpecs,
		ToolChoice:               toolChoice,
		LastTurnToolChoice:       lastTurnToolChoice,
		ParallelToolCalls:        true,
		Search:                   settings.Search,
		ForceExternalSearch:      forceExternalSearch,
		DisableExternalWebFetch:  req.DisableExternalWebFetch,
		MaxTurns:                 settings.MaxTurns,
		MaxOutputTokens:          settings.MaxOutputTokens,
		ServiceTier:              req.ServiceTier,
		ServiceTierSet:           req.ServiceTierSet,
		Debug:                    runtime.debug,
		DebugRaw:                 runtime.debugRaw,
		ApprovalTranscriptPrefix: append([]llm.Message(nil), req.ApprovalTranscriptPrefix...),
	}

	inputMessages := requestInputMessages(req)
	if role := strings.TrimSpace(req.ApprovalRole); role != "" {
		for i := range inputMessages {
			if inputMessages[i].Role == llm.RoleUser {
				inputMessages[i].ApprovalRole = role
			}
		}
	}

	cleanupOnError = false
	return &cmdRunEnvironment{
		cfg:           cfg,
		req:           req,
		runtime:       runtime,
		engine:        engine,
		provider:      provider,
		modelName:     modelName,
		settings:      settings,
		store:         store,
		closeStore:    closeStore,
		sess:          sess,
		llmReq:        llmReq,
		inputMessages: inputMessages,
	}, nil
}

func (r *cmdRunner) resolveSettings(cfg *config.Config, agent *agents.Agent, req runpkg.Request, providerFlag string) (SessionSettings, error) {
	toolsFlag := r.defaults.Tools
	if strings.TrimSpace(req.Tools) != "" {
		toolsFlag = req.Tools
	}
	systemMessage := r.defaults.SystemMessage
	if strings.TrimSpace(req.SystemMessage) != "" {
		systemMessage = req.SystemMessage
	}
	maxTurns := r.defaults.MaxTurns
	maxTurnsSet := false
	if req.MaxTurnsSet || req.MaxTurns > 0 {
		maxTurns = req.MaxTurns
		maxTurnsSet = true
	}
	readDirs := append(append([]string{}, r.defaults.ReadDirs...), req.ReadDirs...)
	writeDirs := append(append([]string{}, r.defaults.WriteDirs...), req.WriteDirs...)
	shellAllow := append([]string{}, r.defaults.ShellAllow...)
	if len(req.ShellAllow) > 0 {
		shellAllow = append(shellAllow, req.ShellAllow...)
	}
	search := r.defaults.Search
	noSearch := r.defaults.NoSearch
	if req.Search != nil {
		search = *req.Search
		noSearch = !*req.Search
	}
	if req.NoSearch {
		search = false
		noSearch = true
	}
	defaultMaxTurns := r.defaults.DefaultMaxTurns
	if defaultMaxTurns <= 0 {
		defaultMaxTurns = 50
	}
	cmdProvider, cmdModel, cmdInstructions, cmdMaxTurns := r.commandConfig(cfg)
	settings, err := ResolveSettingsInDir(cfg, agent, CLIFlags{
		Provider:        providerFlag,
		Tools:           toolsFlag,
		ReadDirs:        readDirs,
		WriteDirs:       writeDirs,
		ShellAllow:      shellAllow,
		MCP:             runnerFirstNonEmpty(req.MCP, r.defaults.MCP),
		SystemMessage:   systemMessage,
		MaxTurns:        maxTurns,
		MaxTurnsSet:     maxTurnsSet,
		MaxOutputTokens: req.MaxOutputTokens,
		Search:          search,
		NoSearch:        noSearch,
		Platform:        templatePlatform(req.Platform),
	}, cmdProvider, cmdModel, cmdInstructions, cmdMaxTurns, defaultMaxTurns, req.Cwd)
	if err != nil {
		return SessionSettings{}, err
	}
	if runCwd := strings.TrimSpace(req.Cwd); runCwd != "" {
		settings.BaseDir = runCwd
		settings.ReadDirs = append(settings.ReadDirs, runCwd)
		settings.WriteDirs = append(settings.WriteDirs, runCwd)
		settings.ShellWorkingDir = runCwd
	}
	return settings, nil
}

func (r *cmdRunner) errWriter() io.Writer {
	if r.defaults.ErrWriter != nil {
		return r.defaults.ErrWriter
	}
	return io.Discard
}

func (r *cmdRunner) commandConfig(cfg *config.Config) (provider, model, instructions string, maxTurns int) {
	if r.defaults.ConfigSet {
		return r.defaults.ConfigProvider, r.defaults.ConfigModel, r.defaults.ConfigInstructions, r.defaults.ConfigMaxTurns
	}
	if cfg == nil {
		return "", "", "", 0
	}
	return cfg.Ask.Provider, cfg.Ask.Model, cfg.Ask.Instructions, cfg.Ask.MaxTurns
}

func configureInteractiveSink(toolMgr *tools.ToolManager, sink runpkg.EventSink) {
	if toolMgr == nil || sink == nil {
		return
	}
	if prompter, ok := sink.(runpkg.ApprovalPrompter); ok {
		toolMgr.ApprovalMgr.PromptUIFunc = prompter.PromptApproval
	}
	if guardian, ok := sink.(runpkg.GuardianEventSink); ok {
		toolMgr.ApprovalMgr.GuardianEventFunc = guardian.GuardianEvent
	}
}

func (r *cmdRunner) ensureRunSession(ctx context.Context, store session.Store, req runpkg.Request, provider llm.Provider, providerKey, modelName, agentName string, settings SessionSettings) *session.Session {
	if store == nil || strings.TrimSpace(req.SessionID) == "" {
		return nil
	}
	if existing, err := store.Get(ctx, req.SessionID); err == nil && existing != nil {
		return existing
	}
	name := strings.TrimSpace(req.SessionName)
	summary := name
	providerName := "unknown"
	if provider != nil {
		if n := strings.TrimSpace(provider.Name()); n != "" {
			providerName = n
		}
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = "unknown"
	}
	sess := &session.Session{
		ID:          req.SessionID,
		Name:        name,
		Summary:     summary,
		Provider:    providerName,
		ProviderKey: strings.TrimSpace(providerKey),
		Model:       modelName,
		Mode:        sessionModeForPlatform(req.Platform),
		Origin:      sessionOriginForPlatform(req.Platform),
		Agent:       agentName,
		ParentID:    strings.TrimSpace(req.ParentSessionID),
		IsSubagent:  req.IsSubagent,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Search:      settings.Search,
		Tools:       settings.Tools,
		MCP:         settings.MCP,
		Status:      session.StatusActive,
	}
	if cwd := strings.TrimSpace(settings.BaseDir); cwd != "" {
		sess.CWD = cwd
	} else if cwd, err := os.Getwd(); err == nil {
		sess.CWD = cwd
	}
	if err := store.Create(ctx, sess); err != nil {
		if existing, getErr := store.Get(ctx, req.SessionID); getErr == nil && existing != nil {
			return existing
		}
		log.Printf("[runner] session Create failed for %s: %v", req.SessionID, err)
		return nil
	}
	return sess
}

func (r *cmdRunner) runProgressive(ctx context.Context, runtime *serveRuntime, engine *llm.Engine, llmReq llm.Request, inputMessages []llm.Message, req runpkg.Request, sess *session.Session, store session.Store, provider llm.Provider, collector *runnerEventCollector) (runpkg.Result, error) {
	if req.Progressive != nil && req.Progressive.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Progressive.Timeout)
		defer cancel()
	}

	messages := append([]llm.Message(nil), inputMessages...)
	if runtime.systemPrompt != "" && !containsSystemMessage(messages) {
		messages = append([]llm.Message{llm.SystemText(runtime.systemPrompt)}, messages...)
	}
	llmReq.Messages = messages

	persistResponseCompleted := req.OnResponseCompleted
	persistTurnCompleted := req.OnTurnCompleted
	persistSyntheticUserMessage := req.OnSyntheticUserMessage
	turnStartTime := time.Now()
	persistStore := store
	if req.DisableRuntimePersistence {
		persistStore = nil
	}
	if persistStore != nil && sess != nil {
		for _, msg := range messages {
			_ = persistStore.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1))
			if msg.Role == llm.RoleUser {
				_ = persistStore.IncrementUserTurns(ctx, sess.ID)
			}
		}
		persistResponseCompleted = func(cbCtx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
			sessionMsg := session.NewMessage(sess.ID, assistantMsg, -1)
			sessionMsg.DurationMs = time.Since(turnStartTime).Milliseconds()
			if err := persistStore.AddMessage(cbCtx, sess.ID, sessionMsg); err != nil {
				return err
			}
			if req.OnResponseCompleted != nil {
				return req.OnResponseCompleted(cbCtx, turnIndex, assistantMsg, metrics)
			}
			return nil
		}
		persistTurnCompleted = func(cbCtx context.Context, turnIndex int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
			for _, msg := range turnMessages {
				sessionMsg := session.NewMessage(sess.ID, msg, -1)
				if msg.Role == llm.RoleAssistant {
					sessionMsg.DurationMs = time.Since(turnStartTime).Milliseconds()
				}
				if err := persistStore.AddMessage(cbCtx, sess.ID, sessionMsg); err != nil {
					return err
				}
			}
			if err := persistStore.UpdateMetrics(cbCtx, sess.ID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens, metrics.CacheWriteTokens); err != nil {
				log.Printf("[runner] session UpdateMetrics failed for %s: %v", sess.ID, err)
			}
			if total, count := engine.ContextEstimateBaseline(); total > 0 {
				if err := persistStore.UpdateContextEstimate(cbCtx, sess.ID, total, count); err != nil {
					log.Printf("[runner] session UpdateContextEstimate failed for %s: %v", sess.ID, err)
				}
			}
			if req.OnTurnCompleted != nil {
				return req.OnTurnCompleted(cbCtx, turnIndex, turnMessages, metrics)
			}
			return nil
		}
		persistSyntheticUserMessage = func(cbCtx context.Context, msg llm.Message) error {
			turnStartTime = time.Now()
			if err := persistStore.AddMessage(cbCtx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
				return err
			}
			if req.OnSyntheticUserMessage != nil {
				return req.OnSyntheticUserMessage(cbCtx, msg)
			}
			return nil
		}
	}

	progressiveResult, err := runProgressiveSession(ctx, engine, llmReq, progressiveRunOptions{
		StopWhen:               progressiveStopWhen(strings.TrimSpace(req.Progressive.StopWhen)),
		ContinueWith:           req.Progressive.ContinueWith,
		SessionID:              req.SessionID,
		ForceNamedFinalization: provider != nil && provider.Capabilities().SupportsToolChoice,
		OnSyntheticUserMessage: persistSyntheticUserMessage,
		OnResponseCompleted:    persistResponseCompleted,
		OnTurnCompleted:        persistTurnCompleted,
		OnEvent: func(ev llm.Event) error {
			return collector.Event(ev)
		},
	})
	if persistStore != nil && sess != nil {
		status := session.StatusComplete
		switch progressiveResult.ExitReason {
		case exitReasonTimeout, exitReasonCancelled:
			status = session.StatusInterrupted
		}
		if err != nil && status != session.StatusInterrupted {
			status = session.StatusError
		}
		_ = persistStore.UpdateStatus(context.Background(), sess.ID, status)
		_ = persistStore.SetCurrent(context.Background(), sess.ID)
	}
	result := collector.Result(req.SessionID)
	if provider != nil {
		result.Provider = provider.Name()
	}
	result.Model = runtime.defaultModel
	result.Engine = engine
	result.ProviderInstance = provider
	result.Progressive = progressiveToRunResult(progressiveResult)
	result.ExitReason = progressiveResult.ExitReason
	result.Response = progressiveOutputText(progressiveResult)
	return result, err
}

func requestInputMessages(req runpkg.Request) []llm.Message {
	if len(req.Messages) > 0 {
		return append([]llm.Message(nil), req.Messages...)
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil
	}
	return []llm.Message{llm.UserText(prompt)}
}

func runnerFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func templatePlatform(platform string) string {
	switch strings.TrimSpace(platform) {
	case runpkg.PlatformJob, "job":
		return "jobs"
	case runpkg.PlatformConsole:
		return "console"
	case runpkg.PlatformWeb:
		return "web"
	case runpkg.PlatformTelegram:
		return "telegram"
	case runpkg.PlatformChat:
		return "chat"
	case runpkg.PlatformExec:
		return "exec"
	default:
		return strings.TrimSpace(platform)
	}
}

func sessionModeForPlatform(platform string) session.SessionMode {
	switch templatePlatform(platform) {
	case "chat", "web", "telegram":
		return session.ModeChat
	case "exec":
		return session.ModeExec
	default:
		return session.ModeAsk
	}
}

func sessionOriginForPlatform(platform string) session.SessionOrigin {
	switch templatePlatform(platform) {
	case "web":
		return session.OriginWeb
	case "telegram":
		return session.OriginTelegram
	default:
		return session.OriginTUI
	}
}

func progressiveToRunResult(result progressiveRunResult) *runpkg.ProgressiveResult {
	return &runpkg.ProgressiveResult{
		ExitReason:    result.ExitReason,
		Finalized:     result.Finalized,
		SessionID:     result.SessionID,
		Sequence:      result.Sequence,
		Reason:        result.Reason,
		Message:       result.Message,
		Progress:      result.Progress,
		FinalResponse: result.FinalResponse,
		FallbackText:  result.FallbackText,
	}
}

func progressiveFromRunResult(result *runpkg.ProgressiveResult) *progressiveRunResult {
	if result == nil {
		return nil
	}
	return &progressiveRunResult{
		ExitReason:    result.ExitReason,
		Finalized:     result.Finalized,
		SessionID:     result.SessionID,
		Sequence:      result.Sequence,
		Reason:        result.Reason,
		Message:       result.Message,
		Progress:      result.Progress,
		FinalResponse: result.FinalResponse,
		FallbackText:  result.FallbackText,
	}
}

type eventSinkFunc func(llm.Event)

func (f eventSinkFunc) Event(ev llm.Event) {
	if f != nil {
		f(ev)
	}
}

type runnerEventCollector struct {
	sink runpkg.EventSink

	thinking       strings.Builder
	thinkingItemID string
	response       strings.Builder
	turns          int
	input          int
	output         int
}

func (c *runnerEventCollector) Event(ev llm.Event) error {
	if c == nil {
		return nil
	}
	switch ev.Type {
	case llm.EventReasoningDelta:
		internalreasoning.AppendStreamItemText(&c.thinking, &c.thinkingItemID, ev.Text, ev.ReasoningItemID)
	case llm.EventTextDelta:
		c.response.WriteString(ev.Text)
	case llm.EventUsage:
		if ev.Use != nil {
			c.turns++
			c.input += ev.Use.InputTokens
			c.output += ev.Use.OutputTokens
		}
	}
	if c.sink != nil {
		if sinkWithError, ok := c.sink.(runpkg.ErrorEventSink); ok {
			return sinkWithError.EventWithError(ev)
		}
		c.sink.Event(ev)
	}
	return nil
}

func (c *runnerEventCollector) Result(sessionID string) runpkg.Result {
	if c == nil {
		return runpkg.Result{SessionID: sessionID}
	}
	return runpkg.Result{
		SessionID:    sessionID,
		Response:     c.response.String(),
		Thinking:     c.thinking.String(),
		Turns:        c.turns,
		InputTokens:  c.input,
		OutputTokens: c.output,
	}
}
