package cmd

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
	"github.com/spf13/cobra"
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

	// Additional completions
	if err := chatCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register tools completion: %v", err))
	}
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	// Extract @agent from args if present
	atAgent, _ := ExtractAgentFromArgs(args)
	if atAgent != "" && chatAgent == "" {
		chatAgent = atAgent
	}

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
	settings := ResolveSettings(cfg, agent, CLIFlags{
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
	engine := llm.NewEngine(provider, defaultToolRegistry(cfg))

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

	// Initialize tools if enabled
	enabledLocalTools := tools.ParseToolsFlag(settings.Tools)
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		if debugLogger != nil {
			debugLogger.Close()
		}
		return err
	}
	if toolMgr != nil {
		// PromptUIFunc will be set up below after tea.Program is created

		// Wire spawn_agent runner if enabled
		if err := WireSpawnAgentRunner(cfg, toolMgr, false); err != nil {
			if debugLogger != nil {
				debugLogger.Close()
			}
			return err
		}
	}

	// Store resolved instructions in config for chat TUI
	cfg.Chat.Instructions = settings.SystemPrompt

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

	// Resolve force external search setting
	forceExternalSearch := resolveForceExternalSearch(cfg, chatNativeSearch, chatNoNativeSearch)

	// Create chat model
	model := chat.New(cfg, provider, engine, modelName, mcpManager, settings.MaxTurns, forceExternalSearch, settings.Search, enabledLocalTools, showStats)

	// Run the TUI (inline mode - no alt screen)
	p := tea.NewProgram(model)

	// Set up the improved approval UI with git-aware heuristics
	if toolMgr != nil {
		toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
			// Flush content and suppress spinner before releasing terminal
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

	// Set up hooks to pause TUI during ask_user prompts
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

	return nil
}

// getModelName extracts the model name from config based on provider
func getModelName(cfg *config.Config) string {
	if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
		return providerCfg.Model
	}
	return "unknown"
}
