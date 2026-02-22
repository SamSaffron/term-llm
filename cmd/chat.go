package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	chatDebug          bool
	chatSearch         bool
	chatProvider       string
	chatMCP            string
	chatMaxTurns       int
	chatNativeSearch   bool
	chatNoNativeSearch bool
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
	// Yolo mode
	chatYolo bool
	// Auto-send mode (for benchmarking) - queue of messages to send
	chatAutoSend []string
	// Text mode (no markdown rendering)
	chatTextMode bool
)

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
  term-llm chat @researcher               # research session
  term-llm chat @agent-builder            # create custom agents
  term-llm chat --agent commit            # alternative syntax

Keyboard shortcuts:
  Enter        - Send message
  Shift+Enter  - Insert newline
  Ctrl+C       - Quit
  Ctrl+K       - Clear conversation
  Ctrl+S       - Toggle web search
  Ctrl+P       - Command palette
  Esc          - Cancel streaming

Slash commands:
  /help        - Show help
  /clear       - Clear conversation
  /model       - Show current model
  /search      - Toggle web search
  /mcp         - Manage MCP servers
  /skills      - List available skills
  /compact     - Compact conversation context
  /quit        - Exit chat`,
	RunE:              runChat,
	ValidArgsFunction: AtAgentCompletion,
}

func init() {
	// Common flags shared across commands
	AddProviderFlag(chatCmd, &chatProvider)
	AddDebugFlag(chatCmd, &chatDebug)
	AddSearchFlag(chatCmd, &chatSearch)
	AddNativeSearchFlags(chatCmd, &chatNativeSearch, &chatNoNativeSearch)
	AddMCPFlag(chatCmd, &chatMCP)
	AddMaxTurnsFlag(chatCmd, &chatMaxTurns, 200) // chat has higher default
	AddToolFlags(chatCmd, &chatTools, &chatReadDirs, &chatWriteDirs, &chatShellAllow)
	AddSystemMessageFlag(chatCmd, &chatSystemMessage)
	AddAgentFlag(chatCmd, &chatAgent)
	AddSkillsFlag(chatCmd, &chatSkills)
	AddYoloFlag(chatCmd, &chatYolo)

	// Auto-send flag for benchmarking (repeatable for multiple messages)
	chatCmd.Flags().StringArrayVar(&chatAutoSend, "auto-send", nil, "Queue message(s) to send automatically and exit after all responses (repeatable)")

	// Text mode flag (no markdown rendering)
	chatCmd.Flags().BoolVar(&chatTextMode, "text", false, "Disable markdown rendering (plain text output)")

	// Session resume flag - NoOptDefVal allows --resume without a value
	chatCmd.Flags().StringVarP(&chatResume, "resume", "r", "", "Resume session (empty for most recent, or session ID)")
	chatCmd.Flags().Lookup("resume").NoOptDefVal = " " // space means "flag was passed without value"

	// Additional completions
	if err := chatCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register tools completion: %v", err))
	}
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	// Extract @agent from args if present, and get remaining args as initial text
	atAgent, filteredArgs := ExtractAgentFromArgs(args)
	if atAgent != "" && chatAgent == "" {
		chatAgent = atAgent
	}
	initialText := strings.Join(filteredArgs, " ")

	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Load agent if specified
	agent, err := LoadAgent(chatAgent, cfg)
	if err != nil {
		return err
	}

	// Resolve all settings: CLI > agent > config
	settings, err := ResolveSettings(cfg, agent, CLIFlags{
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
	}, cfg.Chat.Provider, cfg.Chat.Model, cfg.Chat.Instructions, cfg.Chat.MaxTurns, 200)
	if err != nil {
		return err
	}

	// Initialize session store EARLY so --resume can override settings before tool/MCP setup
	store, storeCleanup := InitSessionStore(cfg, cmd.ErrOrStderr())
	defer storeCleanup()

	// Handle --resume flag BEFORE tool/MCP setup so session settings take effect
	var sess *session.Session
	if cmd.Flags().Changed("resume") {
		if store == nil {
			return fmt.Errorf("session storage is disabled; cannot resume")
		}
		resumeID := strings.TrimSpace(chatResume)
		if resumeID == "" {
			// Resume most recent session
			sess, err = store.GetCurrent(context.Background())
			if err != nil || sess == nil {
				summaries, listErr := store.List(context.Background(), session.ListOptions{Limit: 1})
				if listErr == nil && len(summaries) > 0 {
					sess, _ = store.Get(context.Background(), summaries[0].ID)
				}
			}
			if sess == nil {
				return fmt.Errorf("no session to resume")
			}
		} else {
			sess, err = store.GetByPrefix(context.Background(), resumeID)
			if err != nil {
				return fmt.Errorf("failed to load session: %w", err)
			}
			if sess == nil {
				return fmt.Errorf("session '%s' not found", resumeID)
			}
		}

		// Update current session marker so --resume without ID targets this session
		_ = store.SetCurrent(context.Background(), sess.ID)

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
	}

	// Apply provider overrides
	agentProvider, agentModel := "", ""
	if agent != nil {
		agentProvider, agentModel = agent.Provider, agent.Model
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Chat.Provider, cfg.Chat.Model, chatProvider, agentProvider, agentModel); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider and engine
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
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
	enabledLocalTools := tools.ParseToolsFlag(settings.Tools)
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		if debugLogger != nil {
			debugLogger.Close()
		}
		return err
	}
	if toolMgr != nil {
		// Enable yolo mode if flag is set
		if chatYolo {
			toolMgr.ApprovalMgr.SetYoloMode(true)
		}

		// Register output tool if agent configures one
		if agent != nil && agent.OutputTool.IsConfigured() {
			agentCfg := agent.OutputTool
			param := agentCfg.Param
			if param == "" {
				param = "content"
			}
			toolMgr.Registry.RegisterOutputTool(agentCfg.Name, param, agentCfg.Description)
			toolMgr.SetupEngine(engine)
		}

		// PromptUIFunc will be set up below after tea.Program is created

		// Wire spawn_agent runner if enabled (with session tracking)
		var parentSessionID string
		if sess != nil {
			parentSessionID = sess.ID
		}
		if err := WireSpawnAgentRunnerWithStore(cfg, toolMgr, chatYolo, store, parentSessionID); err != nil {
			if debugLogger != nil {
				debugLogger.Close()
			}
			return err
		}
	}

	// Initialize skills system
	skillsSetup := SetupSkills(&cfg.Skills, chatSkills, cmd.ErrOrStderr())

	RegisterSkillToolWithEngine(engine, toolMgr, skillsSetup)

	// Store resolved instructions in config for chat TUI
	cfg.Chat.Instructions = settings.SystemPrompt

	cfg.Chat.Instructions = InjectSkillsMetadata(cfg.Chat.Instructions, skillsSetup)

	// Determine model name
	modelName := getModelName(cfg)

	// Create MCP manager
	mcpManager := mcp.NewManager()
	if err := mcpManager.LoadConfig(); err != nil {
		// Non-fatal: continue without MCP
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to load MCP config: %v\n", err)
	}

	// Enable MCP servers
	if settings.MCP != "" {
		servers := strings.Split(settings.MCP, ",")
		for _, server := range servers {
			server = strings.TrimSpace(server)
			if server == "" {
				continue
			}
			if err := mcpManager.Enable(ctx, server); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to enable MCP server '%s': %v\n", server, err)
			}
		}
	}

	// Set up MCP sampling provider (for sampling/createMessage requests)
	mcpManager.SetSamplingProvider(provider, modelName, chatYolo)

	// Resolve force external search setting
	forceExternalSearch := resolveForceExternalSearch(cfg, chatNativeSearch, chatNoNativeSearch)

	// Only enable alt-screen when stdout is a terminal (avoid corrupting piped output)
	// Disable alt-screen in auto-send mode for clean output
	autoSendMode := len(chatAutoSend) > 0
	useAltScreen := term.IsTerminal(int(os.Stdout.Fd())) && !autoSendMode

	// Create chat model
	agentName := ""
	if agent != nil {
		agentName = agent.Name
	}
	model := chat.New(cfg, provider, engine, modelName, mcpManager, settings.MaxTurns, forceExternalSearch, settings.Search, enabledLocalTools, settings.Tools, settings.MCP, showStats, initialText, store, sess, useAltScreen, chatAutoSend, true, chatTextMode, agentName, chatYolo)

	// Build program options
	var opts []tea.ProgramOption
	if useAltScreen {
		opts = append(opts, tea.WithAltScreen())
	}
	if autoSendMode {
		// In auto-send mode, don't require TTY input (allows piped/non-interactive use)
		opts = append(opts, tea.WithInput(nil))
	} else {
		opts = append(opts, tea.WithMouseCellMotion()) // Enable mouse support
		opts = append(opts, tea.WithInputTTY())
	}

	// Run the TUI
	p := tea.NewProgram(model, opts...)

	// Set up spawn_agent event callback for subagent progress visibility
	if toolMgr != nil {
		if spawnTool := toolMgr.GetSpawnAgentTool(); spawnTool != nil {
			spawnTool.SetEventCallback(func(callID string, event tools.SubagentEvent) {
				// Use goroutine to avoid blocking subagent execution if TUI message channel backs up
				go p.Send(chat.SubagentProgressMsg{CallID: callID, Event: event})
			})
		}
	}

	// Set up the improved approval UI with git-aware heuristics
	if toolMgr != nil {
		toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
			// In alt screen mode, use inline approval UI
			if useAltScreen {
				// Use buffered channel to prevent goroutine leak if TUI exits before responding
				doneCh := make(chan tools.ApprovalResult, 1)
				p.Send(chat.ApprovalRequestMsg{
					Path:    path,
					IsWrite: isWrite,
					IsShell: isShell,
					DoneCh:  doneCh,
				})
				// Block until user responds or context is cancelled
				select {
				case result := <-doneCh:
					return result, nil
				case <-ctx.Done():
					return tools.ApprovalResult{Choice: tools.ApprovalChoiceDeny}, fmt.Errorf("cancelled: %w", ctx.Err())
				}
			}

			// Inline mode: use external UI with terminal release
			done := make(chan struct{})
			p.Send(chat.FlushBeforeApprovalMsg{Done: done})
			<-done

			// Pause the TUI
			p.ReleaseTerminal()
			defer func() {
				p.RestoreTerminal()
				p.Send(chat.ResumeFromExternalUIMsg{})
			}()

			// Run the appropriate approval UI
			if isShell {
				return tools.RunShellApprovalUI(path)
			}
			return tools.RunFileApprovalUI(path, isWrite)
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

	// Wire signal handling to quit the Bubble Tea program gracefully.
	// This ensures SIGTERM/SIGINT properly exit alt-screen mode.
	go func() {
		<-ctx.Done()
		p.Quit()
	}()

	_, err = p.Run()

	// Cleanup MCP servers
	mcpManager.StopAll()

	// Close debug logger
	if debugLogger != nil {
		debugLogger.Close()
	}

	if err != nil {
		return fmt.Errorf("failed to run chat: %w", err)
	}

	// Print resume hint after alt-screen has been dismissed.
	// Re-fetch the session so we get the latest LLMTurns written during streaming.
	if store != nil && sess != nil && sess.ID != "" {
		if refreshed, fetchErr := store.Get(context.Background(), sess.ID); fetchErr == nil && refreshed != nil && refreshed.LLMTurns >= 1 {
			fmt.Fprintf(os.Stdout, "\n💬 Resume: term-llm chat --resume %s\n", session.ShortID(refreshed.ID))
		}
	}

	return nil
}

// getModelName extracts the model name from config based on provider
func getModelName(cfg *config.Config) string {
	if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
		return providerCfg.Model
	}
	return "unknown"
}
