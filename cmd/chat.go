package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/exitcode"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	chatDebug          bool
	chatSearch         bool
	chatNoSearch       bool
	chatProvider       string
	chatMCP            string
	chatMaxTurns       int
	chatNativeSearch   bool
	chatNoNativeSearch bool
	chatNoWebFetch     bool
	// Tool flags
	chatTools         string
	chatReadDirs      []string
	chatWriteDirs     []string
	chatShellAllow    []string
	chatSystemMessage string
	// Agent flag
	chatAgent string
	// Skills flag
	chatSkills string
	// Session resume flag
	chatResume string
	// Approval modes
	chatApproval             string
	chatYolo                 bool
	chatAutoApproval         bool
	chatHandoverApprovalMode *tools.ApprovalMode
	// Auto-send mode (for benchmarking) - queue of messages to send
	chatAutoSend []string
	// Text mode (no markdown rendering)
	chatTextMode bool
)

var chatOpenTTY = tea.OpenTTY

type chatMCPManager interface {
	SetSamplingProvider(provider llm.Provider, model string, yoloMode bool)
	Enable(ctx context.Context, name string) error
}

func configureChatMCPServers(ctx context.Context, manager chatMCPManager, provider llm.Provider, model string, yoloMode bool, serversCSV string, warnings io.Writer) {
	// Sampling capability is negotiated during MCP initialization, so the handler
	// must exist before the first client is enabled.
	manager.SetSamplingProvider(provider, model, yoloMode)
	for _, server := range strings.Split(serversCSV, ",") {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		if err := manager.Enable(ctx, server); err != nil && warnings != nil {
			fmt.Fprintf(warnings, "Warning: failed to enable MCP server '%s': %v\n", server, err)
		}
	}
}

var chatCmd = &cobra.Command{
	Use:   "chat [@agent]",
	Short: "Start an interactive chat session",
	Long: `Start an interactive TUI chat session with the LLM.

Examples:
  term-llm chat
  term-llm chat -s                        # with web search enabled
  term-llm chat --provider zen            # use specific provider
  term-llm chat --mcp playwright          # with MCP server(s) enabled

Agent examples (use @agent shortcut or --agent flag):
  term-llm chat @reviewer                 # code review session
  term-llm chat @editor                   # code editing session
  term-llm chat @web-researcher             # research session
  term-llm chat @agent-builder            # create custom agents
  term-llm chat --agent commit            # alternative syntax

Keyboard shortcuts:
  Enter        - Send message
  Shift+Enter  - Insert newline
  Ctrl+/ Ctrl+H - Show help
  Ctrl+C       - Copy selection; cancel active response/tool; press twice when idle to quit
  Ctrl+K       - Clear conversation
  Ctrl+N       - New session
  Ctrl+L       - Switch model
  Ctrl+R       - Cycle reasoning effort
  Ctrl+S       - Toggle web search
  Shift+Tab    - Cycle approval mode (prompt/auto/yolo)
  Ctrl+T       - MCP servers (tools)
  Ctrl+O       - Inspect conversation context
  Ctrl+E       - Expand/collapse tool and reasoning details
  Ctrl+P       - Command palette
  Ctrl+Y       - Copy selection, or latest assistant response
  PageUp/Down  - Scroll conversation
  Esc          - Cancel streaming / close modal / clear input

Slash commands:
  /help        - Show help
  /stats       - Show usage, cost, and context breakdown
  /clear       - Clear conversation
  /quit        - Exit chat
  /model       - Switch provider/model
  /effort      - Switch reasoning effort
  /search      - Toggle web search
  /fast        - Toggle ChatGPT fast mode
  /new         - Start a new session
  /save        - Save session with a name
  /copy        - Copy one assistant response as source Markdown
  /export      - Export conversation as markdown
  /thinking    - Toggle reasoning display
  /system      - Set custom system prompt
  /file        - Attach file(s) to next message
  /shell       - Open your shell or run a command in the session directory (--no-rc skips rc files)
  /dirs        - Manage approved directories
  /worktree    - Manage git worktrees for this session
  /mcp         - Manage MCP servers
  /skills      - List available skills
  /inspect     - View conversation/tool details
  /compact     - Compact conversation context
  /resume      - Browse and resume previous sessions
  /tree        - Browse paths or branch from an earlier message
  /reload      - Re-exec current binary and resume session
  /handover    - Hand conversation to another agent`,
	RunE:              runChat,
	ValidArgsFunction: AtAgentCompletion,
}

func init() {
	AddCommonFlags(chatCmd,
		CommonCoreFlags|CommonSearchFlags|CommonMaxTurns|CommonAgent|CommonSkills,
		CommonFlagBindings{
			Provider:        &chatProvider,
			Debug:           &chatDebug,
			Search:          &chatSearch,
			NoSearch:        &chatNoSearch,
			NativeSearch:    &chatNativeSearch,
			NoNativeSearch:  &chatNoNativeSearch,
			NoWebFetch:      &chatNoWebFetch,
			MCP:             &chatMCP,
			MaxTurns:        &chatMaxTurns,
			MaxTurnsDefault: 200,
			Tools:           &chatTools,
			ReadDirs:        &chatReadDirs,
			WriteDirs:       &chatWriteDirs,
			ShellAllow:      &chatShellAllow,
			SystemMessage:   &chatSystemMessage,
			Agent:           &chatAgent,
			Skills:          &chatSkills,
			Approval:        &chatApproval,
			Yolo:            &chatYolo,
			Auto:            &chatAutoApproval,
		})

	// Auto-send flag for benchmarking (repeatable for multiple messages)
	chatCmd.Flags().StringArrayVar(&chatAutoSend, "auto-send", nil, "Queue message(s) to send automatically and exit after all responses (repeatable)")

	// Text mode flag (no markdown rendering)
	chatCmd.Flags().BoolVar(&chatTextMode, "text", false, "Disable markdown rendering (plain text output)")

	// Session resume flag - NoOptDefVal allows --resume without a value
	chatCmd.Flags().StringVarP(&chatResume, "resume", "r", "", "Resume session (empty for most recent, or session ID)")
	chatCmd.Flags().Lookup("resume").NoOptDefVal = " " // space means "flag was passed without value"

	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	if len(chatAutoSend) == 0 && !terminalpolicy.Interactive(os.Stdin, os.Stdout) {
		return fmt.Errorf("chat requires an interactive terminal; use --auto-send for non-interactive execution")
	}

	// Extract @agent from args if present, and get remaining args as initial text
	atAgent, filteredArgs := ExtractAgentFromArgs(args)
	cliAgent := strings.TrimSpace(chatAgent)
	if atAgent != "" && cliAgent == "" {
		cliAgent = atAgent
	}
	initialText := strings.Join(filteredArgs, " ")

	ctx, stop := signal.NotifyContext()
	defer stop()

	resumeRequested := cmd.Flags().Changed("resume")
	resumeID := strings.TrimSpace(chatResume)

	handoverAutoSend := ""
	relaunchHandoff := chatRelaunchHandoff{}
	chatHandoverApprovalMode = nil
	for {
		nextResumeID, nextAutoSend, err := runChatOnce(ctx, cmd, initialText, cliAgent, resumeRequested, resumeID, handoverAutoSend, &relaunchHandoff)
		if err != nil {
			return err
		}
		if nextResumeID == "" {
			return nil
		}

		// Relaunch with full session runtime state restored.
		resumeRequested = true
		resumeID = nextResumeID
		initialText = ""
		handoverAutoSend = nextAutoSend
	}
}

func applyChatSearchDefault(settings *SessionSettings, noSearch bool, sess *session.Session, agentActive bool) {
	if settings == nil || noSearch || sess != nil || agentActive {
		return
	}
	// Bare interactive chat historically exposed web_search/read_url by default.
	// Keep that default visible in the footer and reversible with /search; agents
	// keep their explicit search setting, and --no-search is the explicit opt-out.
	settings.Search = true
}

type chatProgramInput struct {
	reader       io.Reader
	disableInput bool
	cleanup      func()
}

type chatRelaunchHandoff struct {
	branchPrefill   string
	branchPathNotes *chat.BranchPathNotesRequest
}

func sessionIsConversationBranch(ctx context.Context, store session.Store, sessionID string) bool {
	branchStore, ok := store.(session.ConversationBranchStore)
	if !ok || strings.TrimSpace(sessionID) == "" {
		return false
	}
	tree, err := branchStore.GetBranchTree(ctx, sessionID)
	if err != nil {
		return false
	}
	for _, node := range tree.Nodes {
		if node.SessionID == sessionID {
			return strings.TrimSpace(node.ParentSessionID) != ""
		}
	}
	return false
}

func buildChatProgramInput(autoSendMode bool) (chatProgramInput, error) {
	if autoSendMode {
		return chatProgramInput{
			disableInput: true,
			cleanup:      func() {},
		}, nil
	}

	// Keep interactive chat bound to the terminal TTY so redirected stdin can
	// still provide initial content without stealing live keyboard input.
	ttyIn, ttyOut, err := chatOpenTTY()
	if err != nil {
		return chatProgramInput{}, fmt.Errorf("open chat TTY: %w", err)
	}

	return chatProgramInput{
		reader: ttyIn,
		cleanup: func() {
			_ = ttyIn.Close()
			if ttyOut != nil && ttyOut != ttyIn {
				_ = ttyOut.Close()
			}
		},
	}, nil
}

func buildChatHandoverApprovalManager(cfg *config.Config, settings SessionSettings) (*tools.ApprovalManager, error) {
	toolConfig := buildToolConfig("", nil, nil, settings.ShellAllow, cfg)
	perms := tools.NewToolPermissions()
	for _, pattern := range toolConfig.ShellAllow {
		if err := perms.AddShellPattern(pattern); err != nil {
			return nil, err
		}
	}
	for _, script := range settings.Scripts {
		perms.AddScriptCommand(script)
	}
	return tools.NewApprovalManager(perms), nil
}

func runChatOnce(ctx context.Context, cmd *cobra.Command, initialText, cliAgent string, resumeRequested bool, resumeID, handoverAutoSend string, relaunchHandoff *chatRelaunchHandoff) (string, string, error) {
	cfg, err := loadConfigWithSetup()
	if err != nil {
		return "", "", err
	}
	rawConfigInstructions := cfg.Chat.Instructions

	// Initialize session store EARLY so resume can override settings before tool/MCP setup
	store, storeCleanup := InitSessionStore(cfg, cmd.ErrOrStderr())
	var spawnRunner *SpawnAgentRunner
	var finalModel tea.Model
	defer func() {
		// Drain in-flight sub-agent runs before closing the store to avoid
		// "database is closed" warnings from callbacks that outlive the store.
		if spawnRunner != nil {
			spawnRunner.Wait()
		}
		// Wait briefly for the engine stream goroutine before closing the store.
		// Engine callbacks use WithoutCancel and may fire after stream cancellation,
		// but the wait is bounded so a provider/tool that ignores cancellation cannot
		// hang shutdown indefinitely.
		if m, ok := finalModel.(*chat.Model); ok {
			m.WaitStreamDone()
		}
		storeCleanup()
	}()

	var sess *session.Session
	if resumeRequested {
		if store == nil {
			return "", "", fmt.Errorf("session storage is disabled; cannot resume")
		}
		sess, err = resolveChatResumeSession(context.Background(), store, resumeID)
		if err != nil {
			return "", "", err
		}
		_ = store.SetCurrent(context.Background(), sess.ID)
		// Normalize persisted directory metadata before resolving any prompt,
		// project instructions, or skills. A missing worktree falls back to the
		// root/process directory through the same path used for tool binding.
		if err := RestoreWorktreeBinding(context.Background(), store, sess, nil); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to restore session directory: %v\n", err)
		}
	}
	runtimeDir := effectiveSessionDirectory(sess)

	// Saved session agent wins on resume.
	effectiveAgent := strings.TrimSpace(cliAgent)
	if sess != nil {
		effectiveAgent = strings.TrimSpace(sess.Agent)
	}

	agent, err := LoadAgent(effectiveAgent, cfg)
	if err != nil {
		return "", "", err
	}

	// Resolve all settings: CLI > agent > config (resume overrides applied below).
	settings, err := ResolveSettingsInDir(cfg, agent, CLIFlags{
		Provider:      chatProvider,
		Tools:         chatTools,
		ReadDirs:      chatReadDirs,
		WriteDirs:     chatWriteDirs,
		ShellAllow:    chatShellAllow,
		MCP:           chatMCP,
		SystemMessage: chatSystemMessage,
		MaxTurns:      chatMaxTurns,
		MaxTurnsSet:   cmd.Flags().Changed("max-turns"),
		Search:        chatSearch,
		NoSearch:      chatNoSearch,
		Platform:      "chat",
	}, cfg.Chat.Provider, cfg.Chat.Model, rawConfigInstructions, cfg.Chat.MaxTurns, 200, runtimeDir)
	if err != nil {
		return "", "", err
	}
	settings.PrimaryWorkspace = runtimeDir
	applyChatSearchDefault(&settings, chatNoSearch, sess, agent != nil)

	// Saved session settings win on resume.
	if sess != nil {
		settings.Search = sess.Search
		settings.Tools = sess.Tools
		settings.MCP = sess.MCP
		settings.SessionID = sess.ID
	}

	// Resolve requested approval policy before runtime guardian setup so a
	// temporary interactive fallback does not change what the session persists.
	var persistedApprovalMode *session.SessionApprovalMode
	if sess != nil {
		persistedApprovalMode = &sess.ApprovalMode
	}
	cliApprovalMode, err := approvalModeFromCommand(cmd, chatApproval, chatAutoApproval, chatYolo)
	if err != nil {
		return "", "", err
	}
	if cliApprovalMode == nil && chatHandoverApprovalMode != nil {
		carriedMode := *chatHandoverApprovalMode
		cliApprovalMode = &carriedMode
	}
	resolvedApproval, err := resolveApprovalMode(approvalModeResolutionInput{Surface: approvalSurfaceChat, Config: cfg, CLI: cliApprovalMode, Session: persistedApprovalMode})
	if err != nil {
		return "", "", err
	}
	desiredApprovalMode := resolvedApproval.Mode
	resolvedYolo := desiredApprovalMode == tools.ModeYolo
	chatApproval = desiredApprovalMode.String()
	chatYolo = resolvedYolo
	chatAutoApproval = desiredApprovalMode == tools.ModeAuto

	if sess != nil {
		resumeProvider := resolveSessionProviderKey(cfg, sess)
		if resumeProvider == "" {
			resumeProvider = cfg.DefaultProvider
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: unable to infer provider for session %s; falling back to %s\n", session.ShortID(sess.ID), resumeProvider)
		}
		providerOverride := resumeProvider
		if model := strings.TrimSpace(sess.Model); model != "" {
			providerOverride = resumeProvider + ":" + model
		}
		if err := applyProviderOverridesWithAgent(cfg, cfg.Chat.Provider, cfg.Chat.Model, providerOverride, "", ""); err != nil {
			return "", "", err
		}
	} else {
		agentProvider, agentModel := "", ""
		if agent != nil {
			agentProvider, agentModel = agent.Provider, agent.Model
		}
		if err := applyProviderOverridesWithAgent(cfg, cfg.Chat.Provider, cfg.Chat.Model, chatProvider, agentProvider, agentModel); err != nil {
			return "", "", err
		}
	}

	initThemeFromConfig(cfg)

	titleMode, titleModeOK := chat.ParseTerminalTitleMode(cfg.Chat.TerminalTitle)
	if !titleModeOK {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: unknown chat.terminal_title %q; using %q\n", cfg.Chat.TerminalTitle, titleMode)
	}
	cfg.Chat.TerminalTitle = string(titleMode)
	if err := chat.ValidateTerminalTitleFormat(cfg.Chat.TerminalTitleFormat); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: invalid chat.terminal_title_format %q: %v; using default title format\n", cfg.Chat.TerminalTitleFormat, err)
		cfg.Chat.TerminalTitleFormat = ""
	}

	// Create LLM provider and engine
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return "", "", err
	}
	fastProvider, fastErr := llm.NewFastProvider(cfg, cfg.DefaultProvider)
	if fastErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: fast provider setup failed: %v\n", fastErr)
	}
	engine := newEngine(provider, cfg)

	// Set up debug logger if enabled.
	// We close the logger manually after MCP cleanup (not via defer) because
	// MCP servers may still log during shutdown, and the TUI blocks until exit.
	debugLogger, debugLoggerErr := createDebugLogger(cfg)
	if debugLoggerErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", debugLoggerErr)
	}
	if debugLogger != nil {
		engine.SetDebugLogger(debugLogger)
	}

	// Initialize tools if enabled (using possibly-updated settings from resume)
	alignSettingsToActiveProvider(&settings, cfg, provider)
	enabledLocalTools := tools.ParseToolsFlag(settings.Tools)
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		if debugLogger != nil {
			debugLogger.Close()
		}
		return "", "", err
	}
	if toolMgr != nil {
		toolMgr.Registry.SetPlanStore(store)
		workspaceSessionID := ""
		if sess != nil {
			workspaceSessionID = sess.ID
		}
		if err := toolMgr.ConfigureWorkspacePersistence(context.Background(), store, workspaceSessionID); err != nil {
			return "", "", err
		}
	}
	if sess != nil && toolMgr != nil {
		if err := RestoreWorktreeBinding(context.Background(), store, sess, toolMgr); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to restore worktree binding: %v\n", err)
		}
	}
	approvalMgr, err := buildChatHandoverApprovalManager(cfg, settings)
	if err != nil {
		if debugLogger != nil {
			debugLogger.Close()
		}
		return "", "", err
	}
	// Error-path safety net. The normal exit path closes Guardian explicitly
	// before the debug logger so provider cleanup can still emit diagnostics.
	defer func() {
		if approvalMgr != nil {
			approvalMgr.Close()
		}
	}()
	if toolMgr != nil {
		approvalMgr = toolMgr.ApprovalMgr
		if err := applyResolvedApprovalMode(cfg, approvalMgr, resolvedApproval, cfg.DefaultProvider, getModelName(cfg), approvalRuntimeOptions{
			PrepareCallbacks: true,
			WarningWriter:    cmd.ErrOrStderr(),
		}); err != nil {
			return "", "", err
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
		spawnRunner, wireErr = WireSpawnAgentRunnerWithStore(cfg, toolMgr, resolvedYolo, store, parentSessionID)
		if wireErr != nil {
			if debugLogger != nil {
				debugLogger.Close()
			}
			return "", "", wireErr
		}
	} else {
		if err := applyResolvedApprovalMode(cfg, approvalMgr, resolvedApproval, cfg.DefaultProvider, getModelName(cfg), approvalRuntimeOptions{
			PrepareCallbacks: true,
			WarningWriter:    cmd.ErrOrStderr(),
		}); err != nil {
			return "", "", err
		}
	}
	reportApprovalMode(cmd.ErrOrStderr(), chatDebug, resolvedApproval, approvalMgr)

	// Initialize skills system
	agentSkills := ""
	if agent != nil {
		agentSkills = agent.Skills
	}
	skillsSetup := SetupSkillsInDir(&cfg.Skills, chatSkills, agentSkills, cmd.ErrOrStderr(), runtimeDir)

	// Store resolved instructions in config for chat TUI
	cfg.Chat.Instructions = InjectSkillsMetadata(settings.SystemPrompt, skillsSetup)

	RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	// Direct isolated skills use the generic child runner even when the current
	// agent does not expose the model-facing spawn_agent tool.
	if skillsSetup != nil && spawnRunner == nil {
		parentSessionID := ""
		if sess != nil {
			parentSessionID = sess.ID
		}
		spawnRunner, err = NewSpawnAgentRunnerWithStore(cfg, resolvedYolo, approvalMgr, store, parentSessionID)
		if err != nil {
			return "", "", fmt.Errorf("initialize isolated skill runner: %w", err)
		}
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
		modelName = extractModelFromProviderName(provider.Name())
	}
	providerKey := cfg.DefaultProvider

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
		sess.ApprovalMode = approvalModeToSession(desiredApprovalMode)
		_ = store.Update(context.Background(), sess)
	}

	// Create MCP manager
	mcpManager := mcp.NewManager()
	if err := mcpManager.LoadConfig(); err != nil {
		// Non-fatal: continue without MCP
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to load MCP config: %v\n", err)
	}

	configureChatMCPServers(ctx, mcpManager, provider, modelName, resolvedYolo, settings.MCP, cmd.ErrOrStderr())

	// Resolve force external search setting
	forceExternalSearch := resolveForceExternalSearch(cfg, chatNativeSearch, chatNoNativeSearch)

	// Only enable alt-screen when stdout is a terminal (avoid corrupting piped output)
	// Disable alt-screen in auto-send mode for clean output
	autoSendMode := len(chatAutoSend) > 0
	useAltScreen := term.IsTerminal(int(os.Stdout.Fd())) && !autoSendMode

	// Create chat model
	chatPlatformMessage := ""
	if agent != nil {
		chatPlatformMessage = agent.PlatformMessages.For("chat")
	}
	model := chat.NewWithFastProviderAndApproval(cfg, provider, fastProvider, engine, providerKey, modelName, mcpManager, settings.MaxTurns, forceExternalSearch, chatNoWebFetch, settings.Search, enabledLocalTools, settings.Tools, settings.MCP, false, initialText, store, sess, useAltScreen, chatAutoSend, autoSendMode, chatTextMode, agentName, chatPlatformMessage, resolvedYolo, desiredApprovalMode, toolMgr)
	model.SetAgentMentionCapability(runtimeAgentMentionCapability{engine: model.CurrentAgentMentionEngine, manager: toolMgr})
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
	defer restoreTerminalTitle()
	if agent != nil && agent.OutputTool.IsConfigured() {
		model.SetFooterWarning("agent output_tool is ignored in chat; use ask for tool-captured output")
	}
	model.SetRootContext(ctx)
	model.SetRunner(newCmdRunner(cfg, cmdRunnerOptions{
		Provider:           chatProvider,
		ConfigSet:          true,
		ConfigProvider:     cfg.Chat.Provider,
		ConfigModel:        cfg.Chat.Model,
		ConfigInstructions: cfg.Chat.Instructions,
		ConfigMaxTurns:     cfg.Chat.MaxTurns,
		Tools:              settings.Tools,
		ReadDirs:           append([]string(nil), chatReadDirs...),
		WriteDirs:          append([]string(nil), chatWriteDirs...),
		ShellAllow:         append([]string(nil), chatShellAllow...),
		MCP:                settings.MCP,
		MaxTurns:           settings.MaxTurns,
		DefaultMaxTurns:    200,
		Search:             settings.Search,
		NoSearch:           chatNoSearch,
		NativeSearch:       chatNativeSearch,
		NoNativeSearch:     chatNoNativeSearch,
		ApprovalMode:       resolvedApproval.Mode,
		ApprovalModeSet:    true,
		ApprovalSource:     resolvedApproval.Source,
		Debug:              chatDebug,
		DebugRaw:           debugRaw,
		ErrWriter:          cmd.ErrOrStderr(),
		Store:              store,
		ParentApprovalMgr:  approvalMgr,
	}))
	model.SetChildRunner(spawnRunner)

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
	}
	model.SetSideQuestionProviderFactory(func(providerKey, modelName string) (llm.Provider, error) {
		if strings.TrimSpace(providerKey) == "" {
			providerKey = provider.Name()
		}
		return llm.NewProviderByName(cfg, providerKey, modelName)
	})
	model.SetHandoverApprovalManager(approvalMgr)
	model.PersistApprovalMode(desiredApprovalMode)

	// Wire agent resolver, lister, and current agent for /handover support
	model.SetAgentResolver(LoadAgent)
	currentRuntimeContext := chat.RuntimeSystemContext{
		SystemPrompt: cfg.Chat.Instructions,
		ApplySkills:  skillContextApplier(skillsSetup),
		Skills:       skillsSetup,
	}
	model.SetRuntimeSystemContextResolver(func(targetAgent *agents.Agent, providerKey, modelName, dir string) (chat.RuntimeSystemContext, error) {
		systemMessage := chatSystemMessage
		if targetAgent != agent {
			systemMessage = ""
		}
		return resolveChatRuntimeSystemContextWithConfig(cmd, cfg, targetAgent, providerKey, modelName, dir, rawConfigInstructions, systemMessage)
	}, currentRuntimeContext)
	model.SetHandoverSystemPromptResolver(func(targetAgent *agents.Agent, providerKey, modelName string) (string, error) {
		return resolveChatHandoverSystemPrompt(cmd, targetAgent, providerKey, modelName)
	})
	model.SetGuardianReviewerRefresh(func(providerKey, modelName string) error {
		return installGuardianReviewerCallbacks(cfg, approvalMgr, providerKey, modelName, false)
	})
	model.SetAgentLister(ListAgentNames)
	if agent != nil {
		model.SetCurrentAgent(agent)
	}

	// Build program options. AltScreen and mouse mode are declarative on the View in v2.
	programInput, err := buildChatProgramInput(autoSendMode)
	if err != nil {
		return "", "", err
	}
	defer programInput.cleanup()

	var opts []tea.ProgramOption
	opts = append(opts, tea.WithoutSignalHandler())
	if programInput.disableInput {
		opts = append(opts, tea.WithInput(nil))
	} else if programInput.reader != nil {
		opts = append(opts, tea.WithInput(programInput.reader))
	}

	// Run the TUI. Image bytes and their acknowledgements are attached directly
	// to the tea.View that composed them; stdout remains renderer-owned.
	p := tea.NewProgram(model, opts...)
	model.SetProgram(p)

	// Set up spawn_agent event callback for subagent progress visibility
	if toolMgr != nil {
		if spawnTool := toolMgr.GetSpawnAgentTool(); spawnTool != nil {
			dispatcher := newSubagentProgressDispatcher(func(callID string, event tools.SubagentEvent) {
				p.Send(chat.SubagentProgressMsg{CallID: callID, Event: event})
			})
			spawnTool.SetEventCallback(dispatcher.Callback)
		}
	}

	// Set up the improved approval UI with git-aware heuristics.
	// This also powers /handover script approvals even when no shell tool is enabled.
	if approvalMgr != nil {
		approvalMgr.GuardianEventFunc = func(event tools.GuardianEvent) {
			p.Send(chat.GuardianReviewMsg{Event: event})
		}
		approvalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (tools.ApprovalResult, error) {
			// In alt screen mode, use inline approval UI.
			if useAltScreen {
				doneCh := make(chan tools.ApprovalResult, 1)
				p.Send(chat.ApprovalRequestMsg{
					Path: path, IsWrite: isWrite, IsShell: isShell, WorkDir: workDir, DoneCh: doneCh,
				})
				select {
				case result := <-doneCh:
					return result, nil
				case <-ctx.Done():
					return tools.ApprovalResult{Choice: tools.ApprovalChoiceDeny}, fmt.Errorf("cancelled: %w", ctx.Err())
				}
			}

			// Inline mode: use external UI with terminal release.
			done := make(chan struct{})
			p.Send(chat.FlushBeforeApprovalMsg{Done: done})
			<-done
			p.ReleaseTerminal()
			defer func() {
				p.RestoreTerminal()
				p.Send(chat.ResumeFromExternalUIMsg{})
			}()
			if isShell {
				return tools.RunShellApprovalUI(path, workDir)
			}
			return tools.RunFileApprovalUI(path, isWrite)
		}
		approvalMgr.WorkspacePromptFunc = func(workspace string) (tools.WorkspaceApprovalResult, error) {
			if useAltScreen {
				doneCh := make(chan tools.ApprovalResult, 1)
				p.Send(chat.ApprovalRequestMsg{Path: workspace, IsWorkspace: true, DoneCh: doneCh})
				select {
				case result := <-doneCh:
					choice := result.Choice
					return tools.WorkspaceApprovalResult{
						Approved:  !result.Cancelled && (choice == tools.ApprovalChoiceWorkspace || choice == tools.ApprovalChoiceWorkspaceRemember),
						Remember:  !result.Cancelled && choice == tools.ApprovalChoiceWorkspaceRemember,
						Cancelled: result.Cancelled,
					}, nil
				case <-ctx.Done():
					return tools.WorkspaceApprovalResult{Cancelled: true}, fmt.Errorf("cancelled: %w", ctx.Err())
				}
			}

			done := make(chan struct{})
			p.Send(chat.FlushBeforeApprovalMsg{Done: done})
			<-done
			p.ReleaseTerminal()
			defer func() {
				p.RestoreTerminal()
				p.Send(chat.ResumeFromExternalUIMsg{})
			}()
			return tools.RunWorkspaceApprovalUI(workspace)
		}
	}

	// Set up ask_user handling
	if useAltScreen {
		// In alt screen mode, use inline rendering
		tools.SetAskUserUIFunc(func(questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
			// Use buffered channel to prevent goroutine leak if TUI exits before responding
			doneCh := make(chan []tools.AskUserAnswer, 1)
			p.Send(chat.AskUserRequestMsg{
				Questions: questions,
				DoneCh:    doneCh,
			})
			// Block until user responds or context is cancelled
			select {
			case answers := <-doneCh:
				if answers == nil {
					return nil, fmt.Errorf("cancelled by user")
				}
				return answers, nil
			case <-ctx.Done():
				return nil, fmt.Errorf("cancelled: %w", ctx.Err())
			}
		})
		defer tools.ClearAskUserUIFunc()
	} else {
		// In inline mode, use external UI with hooks
		start, end := tools.CreateTUIHooks(p, func() {
			done := make(chan struct{})
			p.Send(chat.FlushBeforeAskUserMsg{Done: done})
			<-done
		})
		// Wrap end hook to also send resume message after terminal is restored
		originalEnd := end
		end = func() {
			originalEnd()
			p.Send(chat.ResumeFromExternalUIMsg{})
		}
		tools.SetAskUserHooks(start, end)
		defer tools.ClearAskUserHooks()
	}

	// Set up initiate_handover handling — works in both alt screen and inline modes
	// because cmdHandover already handles both.
	tools.SetHandoverUIFunc(func(toolCtx context.Context, agent string) (bool, error) {
		doneCh := make(chan bool, 1)
		p.Send(chat.HandoverRequestMsg{
			Agent:  agent,
			DoneCh: doneCh,
		})
		select {
		case confirmed := <-doneCh:
			return confirmed, nil
		case <-toolCtx.Done():
			return false, toolCtx.Err()
		}
	})
	defer tools.ClearHandoverUIFunc()

	// Wire OS signal handling to kill the Bubble Tea program and restore the
	// terminal. Ctrl+C in raw TUI mode is handled as a keypress by the model;
	// this path covers real SIGINT/SIGTERM (for example from another terminal).
	go func() {
		<-ctx.Done()
		killed := make(chan struct{})
		go func() {
			p.Kill()
			close(killed)
		}()
		select {
		case <-killed:
		case <-time.After(2 * time.Second):
			fmt.Fprintln(os.Stderr, "term-llm: forced exit after interrupt")
			os.Exit(130)
		}
	}()

	finalModel, err = p.Run()
	restoreTerminalTitle()

	// Cleanup MCP servers and Guardian-owned providers before closing logging.
	mcpManager.StopAll()
	approvalMgr.Close()

	// Close debug logger
	if debugLogger != nil {
		debugLogger.Close()
	}

	if err != nil {
		if ctx.Err() != nil && errors.Is(err, tea.ErrProgramKilled) {
			return "", "", exitcode.Cancel()
		}
		return "", "", fmt.Errorf("failed to run chat: %w", err)
	}

	var nextResumeID, nextHandoverAutoSend string
	if m, ok := finalModel.(*chat.Model); ok {
		nextResumeID = m.RequestedResumeSessionID()
		nextHandoverAutoSend = m.RequestedHandoverAutoSend()
		if relaunchHandoff != nil {
			relaunchHandoff.branchPrefill = m.RequestedBranchPrefill()
			relaunchHandoff.branchPathNotes = m.RequestedBranchPathNotes()
		}
		// Carry a user-selected mode only into an actual handover. Ordinary /resume
		// and /new relaunches must resolve the target session/config independently.
		mode := m.ApprovalModeRequested()
		chatHandoverApprovalMode = chatApprovalCarryForRelaunch(m.ApprovalModeChanged(), nextHandoverAutoSend, mode)
		chatApproval = mode.String()
		chatYolo = mode == tools.ModeYolo
		chatAutoApproval = mode == tools.ModeAuto
	}

	// Handle /reload: close the store, then re-exec under the (potentially new) binary.
	if m, ok := finalModel.(*chat.Model); ok && m.WantsReload() {
		if spawnRunner != nil {
			spawnRunner.Wait()
		}
		storeCleanup() // flush & close DB before replacing the process
		sessionID := m.ReloadSessionID()
		if execErr := execReload(sessionID); execErr != nil {
			// exec failed (shouldn't happen on Unix) — fall through and exit normally
			fmt.Fprintf(cmd.ErrOrStderr(), "reload: %v\n", execErr)
		}
		return "", "", nil
	}

	// Print resume hint after alt-screen has been dismissed.
	// Re-fetch the session so we get the latest LLMTurns written during streaming.
	if nextResumeID == "" && store != nil && sess != nil && sess.ID != "" {
		if refreshed, fetchErr := store.Get(context.Background(), sess.ID); fetchErr == nil && refreshed != nil && refreshed.LLMTurns >= 1 {
			fmt.Fprintf(os.Stdout, "\n💬 Resume: %s\n", chatResumeCommand(refreshed))
		}
	}

	return nextResumeID, nextHandoverAutoSend, nil
}

func resolveChatHandoverSystemPrompt(cmd *cobra.Command, targetAgent *agents.Agent, providerKey, modelName string) (string, error) {
	cfg, err := loadConfigWithSetup()
	if err != nil {
		return "", err
	}
	return resolveChatHandoverSystemPromptWithConfig(cmd, cfg, targetAgent, providerKey, modelName)
}

func resolveChatHandoverSystemPromptWithConfig(cmd *cobra.Command, cfg *config.Config, targetAgent *agents.Agent, providerKey, modelName string) (string, error) {
	if targetAgent == nil {
		return "", nil
	}
	resolved, err := resolveChatRuntimeSystemContextWithConfig(cmd, cfg, targetAgent, providerKey, modelName, "", cfg.Chat.Instructions, "")
	return resolved.SystemPrompt, err
}

func resolveChatRuntimeSystemContextWithConfig(cmd *cobra.Command, cfg *config.Config, targetAgent *agents.Agent, providerKey, modelName, runtimeDir, rawConfigInstructions, systemMessage string) (chat.RuntimeSystemContext, error) {
	if cfg == nil {
		cfg = &config.Config{}
	}

	var promptAgent *agents.Agent
	agentSkills := ""
	if targetAgent != nil {
		copyAgent := *targetAgent
		copyAgent.Provider = strings.TrimSpace(providerKey)
		copyAgent.Model = strings.TrimSpace(modelName)
		promptAgent = &copyAgent
		agentSkills = copyAgent.Skills
	}

	maxTurnsSet := false
	errWriter := io.Discard
	if cmd != nil {
		maxTurnsSet = cmd.Flags().Changed("max-turns")
		errWriter = cmd.ErrOrStderr()
	}

	settings, err := ResolveSettingsInDir(cfg, promptAgent, CLIFlags{
		Provider:        "",
		Tools:           chatTools,
		ReadDirs:        chatReadDirs,
		WriteDirs:       chatWriteDirs,
		ShellAllow:      chatShellAllow,
		MCP:             chatMCP,
		SystemMessage:   systemMessage,
		MaxTurns:        chatMaxTurns,
		MaxTurnsSet:     maxTurnsSet,
		Search:          chatSearch,
		NoSearch:        chatNoSearch,
		MaxOutputTokens: 0,
		Platform:        "chat",
	}, providerKey, modelName, rawConfigInstructions, cfg.Chat.MaxTurns, 200, runtimeDir)
	if err != nil {
		return chat.RuntimeSystemContext{}, err
	}

	skillsSetup := SetupSkillsInDir(&cfg.Skills, chatSkills, agentSkills, errWriter, runtimeDir)
	return chat.RuntimeSystemContext{
		SystemPrompt: InjectSkillsMetadata(settings.SystemPrompt, skillsSetup),
		ApplySkills:  skillContextApplier(skillsSetup),
		Skills:       skillsSetup,
	}, nil
}

func skillContextApplier(setup *skills.Setup) func(*llm.Engine, *tools.ToolManager) {
	return func(engine *llm.Engine, toolMgr *tools.ToolManager) {
		if engine == nil {
			return
		}
		engine.UnregisterTool(tools.ActivateSkillToolName)
		engine.UnregisterTool(tools.SearchSkillsToolName)
		RegisterSkillToolWithEngine(engine, toolMgr, setup)
	}
}

func effectiveSessionDirectory(sess *session.Session) string {
	if sess != nil {
		if dir := strings.TrimSpace(sess.WorktreeDir); dir != "" {
			return dir
		}
		// Web/telegram sessions historically recorded the daemon process CWD.
		// A local interactive resume must not treat that as user-selected state.
		if sess.Origin != session.OriginWeb && sess.Origin != session.OriginTelegram {
			if dir := strings.TrimSpace(sess.CWD); dir != "" {
				return dir
			}
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

func chatResumeCommand(sess *session.Session) string {
	resumeID := ""
	if sess != nil {
		if sess.Number > 0 {
			resumeID = strconv.FormatInt(sess.Number, 10)
		} else {
			id := strings.TrimSpace(sess.ID)
			resumeID = id
			if !session.ParseIDTime(id).IsZero() {
				resumeID = session.ShortID(id)
			}
		}
	}
	return "term-llm chat --resume=" + resumeID
}

func resolveChatResumeSession(ctx context.Context, store session.Store, resumeID string) (*session.Session, error) {
	resumeID = strings.TrimSpace(resumeID)
	if resumeID == "" {
		sess, err := store.GetCurrent(ctx)
		if err == nil && sess != nil {
			return sess, nil
		}
		summaries, listErr := store.List(ctx, session.ListOptions{Limit: 1})
		if listErr != nil {
			return nil, fmt.Errorf("failed to list sessions: %w", listErr)
		}
		if len(summaries) == 0 {
			return nil, fmt.Errorf("no session to resume")
		}
		sess, err = store.Get(ctx, summaries[0].ID)
		if err != nil {
			return nil, fmt.Errorf("failed to load session: %w", err)
		}
		if sess == nil {
			return nil, fmt.Errorf("no session to resume")
		}
		return sess, nil
	}

	sess, err := store.GetByPrefix(ctx, resumeID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("session '%s' not found", resumeID)
	}
	return sess, nil
}

func resolveSessionProviderKey(cfg *config.Config, sess *session.Session) string {
	if sess == nil || cfg == nil {
		return ""
	}

	resolveKnownProvider := func(candidate string) string {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return ""
		}
		if candidate == "debug" {
			return candidate
		}
		for key := range cfg.Providers {
			if strings.EqualFold(candidate, key) {
				return key
			}
		}
		for _, builtIn := range llm.GetBuiltInProviderNames() {
			if strings.EqualFold(candidate, builtIn) {
				return builtIn
			}
		}
		return ""
	}

	if known := resolveKnownProvider(sess.ProviderKey); known != "" {
		return known
	}

	display := strings.TrimSpace(sess.Provider)
	if display == "" {
		return ""
	}
	lower := strings.ToLower(display)

	// Custom providers include the provider key in Name() prefix: "<key> (<model>)"
	for key := range cfg.Providers {
		lowerKey := strings.ToLower(key)
		if lower == lowerKey || strings.HasPrefix(lower, lowerKey+" (") {
			return key
		}
	}
	for _, builtIn := range llm.GetBuiltInProviderNames() {
		lowerBuiltIn := strings.ToLower(builtIn)
		if lower == lowerBuiltIn || strings.HasPrefix(lower, lowerBuiltIn+" (") {
			return builtIn
		}
	}

	switch {
	case strings.HasPrefix(lower, "github copilot ("):
		return "copilot"
	case strings.HasPrefix(lower, "claude cli ("):
		return "claude-bin"
	case strings.HasPrefix(lower, "grok cli ("):
		return "grok-bin"
	case strings.HasPrefix(lower, "cursor cli ("):
		return "cursor-bin"
	case lower == "agy cli" || strings.HasPrefix(lower, "agy cli ("):
		return "agy-bin"
	case strings.HasPrefix(lower, "debug"):
		return "debug"
	default:
		return ""
	}
}

// getModelName returns the configured model; "" means caller should fall back to extractModelFromProviderName(provider.Name()).
func getModelName(cfg *config.Config) string {
	if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
		return providerCfg.Model
	}
	return ""
}

// extractModelFromProviderName parses "<Provider> (<model>[, ...])" Name() strings shared by all providers.
func extractModelFromProviderName(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), "agy CLI") {
		// Unlike the other CLI providers, agy intentionally leaves its model empty
		// so the CLI can select its own default. Do not treat the display label as
		// a model name and pass `--model "agy CLI"` back to agy.
		return ""
	}
	open := strings.Index(name, "(")
	if open < 0 {
		return name
	}
	rest := name[open+1:]
	close := strings.Index(rest, ")")
	if close < 0 {
		return name
	}
	inner := rest[:close]
	if comma := strings.Index(inner, ","); comma >= 0 {
		inner = inner[:comma]
	}
	return strings.TrimSpace(inner)
}
