package cmd

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/agents"
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
	Use:   "chat",
	Short: "Start an interactive chat session",
	Long: `Start an interactive TUI chat session with the LLM.

Examples:
  term-llm chat
  term-llm chat -s                        # with web search enabled
  term-llm chat --provider zen            # use specific provider
  term-llm chat --mcp playwright          # with MCP server(s) enabled
  term-llm chat --agent reviewer          # with agent configuration

Agent examples:
  term-llm chat --agent reviewer          # code review session
  term-llm chat -a editor                 # code editing session
  term-llm chat -a researcher             # research session

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
	RunE: runChat,
}

func init() {
	chatCmd.Flags().BoolVarP(&chatSearch, "search", "s", false, "Enable web search")
	chatCmd.Flags().BoolVarP(&chatDebug, "debug", "d", false, "Show debug information")
	chatCmd.Flags().StringVar(&chatProvider, "provider", "", "Override provider, optionally with model (e.g., openai:gpt-4o)")
	chatCmd.Flags().StringVar(&chatMCP, "mcp", "", "Enable MCP server(s), comma-separated (e.g., playwright,filesystem)")
	chatCmd.Flags().IntVar(&chatMaxTurns, "max-turns", 200, "Max agentic turns for tool execution")
	chatCmd.Flags().BoolVar(&chatNativeSearch, "native-search", false, "Use provider's native search (override config)")
	chatCmd.Flags().BoolVar(&chatNoNativeSearch, "no-native-search", false, "Use external search tools instead of provider's native search")
	// Tool flags
	chatCmd.Flags().StringVar(&chatTools, "tools", "", "Enable local tools (comma-separated, or 'all' for everything: read,write,edit,shell,grep,find,view,image)")
	chatCmd.Flags().StringArrayVar(&chatReadDirs, "read-dir", nil, "Directories for read/grep/find/view tools (repeatable)")
	chatCmd.Flags().StringArrayVar(&chatWriteDirs, "write-dir", nil, "Directories for write/edit tools (repeatable)")
	chatCmd.Flags().StringArrayVar(&chatShellAllow, "shell-allow", nil, "Shell command patterns to allow (repeatable, glob syntax)")
	chatCmd.Flags().StringVarP(&chatSystemMessage, "system-message", "m", "", "System message/instructions for the LLM (overrides config)")
	chatCmd.Flags().StringVarP(&chatAgent, "agent", "a", "", "Use an agent (named configuration bundle)")
	if err := chatCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register provider completion: %v", err))
	}
	if err := chatCmd.RegisterFlagCompletionFunc("mcp", MCPFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register mcp completion: %v", err))
	}
	if err := chatCmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register tools completion: %v", err))
	}
	if err := chatCmd.RegisterFlagCompletionFunc("agent", AgentFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register agent completion: %v", err))
	}
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext()
	defer stop()

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Load agent if specified
	var agent *agents.Agent
	if chatAgent != "" {
		registry, err := agents.NewRegistry(agents.RegistryConfig{
			UseBuiltin:  cfg.Agents.UseBuiltin,
			SearchPaths: cfg.Agents.SearchPaths,
		})
		if err != nil {
			return fmt.Errorf("create agent registry: %w", err)
		}

		agent, err = registry.Get(chatAgent)
		if err != nil {
			return fmt.Errorf("load agent: %w", err)
		}

		if err := agent.Validate(); err != nil {
			return fmt.Errorf("invalid agent: %w", err)
		}
	}

	// Apply provider overrides: CLI > agent > config
	agentProvider := ""
	agentModel := ""
	if agent != nil {
		agentProvider = agent.Provider
		agentModel = agent.Model
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Chat.Provider, cfg.Chat.Model, chatProvider, agentProvider, agentModel); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Determine max turns: CLI > agent > config
	maxTurns := chatMaxTurns
	if !cmd.Flags().Changed("max-turns") {
		if agent != nil && agent.MaxTurns > 0 {
			maxTurns = agent.MaxTurns
		} else if cfg.Chat.MaxTurns > 0 {
			maxTurns = cfg.Chat.MaxTurns
		}
	}

	engine := llm.NewEngine(provider, defaultToolRegistry(cfg))

	// Determine tool settings: CLI > agent > none
	effectiveTools := chatTools
	effectiveReadDirs := chatReadDirs
	effectiveWriteDirs := chatWriteDirs
	effectiveShellAllow := chatShellAllow
	shellAutoRun := false
	var scriptCommands []string

	if agent != nil && effectiveTools == "" {
		// Use agent tool settings
		if agent.HasEnabledList() {
			effectiveTools = strings.Join(agent.Tools.Enabled, ",")
		} else if agent.HasDisabledList() {
			// Get all tools and exclude disabled ones
			allTools := tools.AllToolNames()
			enabledTools := agent.GetEnabledTools(allTools)
			effectiveTools = strings.Join(enabledTools, ",")
		}

		// Agent-specific tool settings
		if len(agent.Read.Dirs) > 0 {
			effectiveReadDirs = agent.Read.Dirs
		}
		if len(agent.Shell.Allow) > 0 {
			effectiveShellAllow = agent.Shell.Allow
		}
		shellAutoRun = agent.Shell.AutoRun

		// Extract script commands from agent
		if len(agent.Shell.Scripts) > 0 {
			for _, script := range agent.Shell.Scripts {
				scriptCommands = append(scriptCommands, script)
			}
		}
	}

	// Initialize local tools if we have any
	var enabledLocalTools []string
	if effectiveTools != "" {
		toolConfig := buildToolConfig(effectiveTools, effectiveReadDirs, effectiveWriteDirs, effectiveShellAllow, cfg)
		if shellAutoRun {
			toolConfig.ShellAutoRun = true
		}
		if len(scriptCommands) > 0 {
			toolConfig.ScriptCommands = append(toolConfig.ScriptCommands, scriptCommands...)
		}
		if errs := toolConfig.Validate(); len(errs) > 0 {
			return fmt.Errorf("invalid tool config: %v", errs[0])
		}
		enabledLocalTools = toolConfig.Enabled
		toolMgr, err := tools.NewToolManager(&toolConfig, cfg)
		if err != nil {
			return fmt.Errorf("failed to initialize tools: %w", err)
		}
		toolMgr.ApprovalMgr.PromptFunc = tools.HuhApprovalPrompt
		toolMgr.SetupEngine(engine)
	}

	// Determine system instructions: CLI > agent > config
	instructions := cfg.Chat.Instructions
	if agent != nil && agent.SystemPrompt != "" {
		// Expand template variables in agent system prompt
		templateCtx := agents.NewTemplateContext()

		// Extract resources for builtin agents and set resource_dir
		if agents.IsBuiltinAgent(agent.Name) {
			if resourceDir, err := agents.ExtractBuiltinResources(agent.Name); err == nil {
				templateCtx = templateCtx.WithResourceDir(resourceDir)
			}
		}

		instructions = agents.ExpandTemplate(agent.SystemPrompt, templateCtx)
	}
	if chatSystemMessage != "" {
		instructions = chatSystemMessage
	}
	cfg.Chat.Instructions = instructions

	// Determine model name
	modelName := getModelName(cfg)

	// Create MCP manager
	mcpManager := mcp.NewManager()
	if err := mcpManager.LoadConfig(); err != nil {
		// Non-fatal: continue without MCP
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to load MCP config: %v\n", err)
	}

	// Determine MCP servers: CLI > agent
	effectiveMCP := chatMCP
	if agent != nil && effectiveMCP == "" {
		mcpServers := agent.GetMCPServerNames()
		if len(mcpServers) > 0 {
			effectiveMCP = strings.Join(mcpServers, ",")
		}
	}

	// Enable MCP servers
	if effectiveMCP != "" {
		servers := strings.Split(effectiveMCP, ",")
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

	// Resolve force external search setting from config and flags
	forceExternalSearch := resolveForceExternalSearch(cfg, chatNativeSearch, chatNoNativeSearch)

	// Create chat model
	model := chat.New(cfg, provider, engine, modelName, mcpManager, maxTurns, forceExternalSearch, chatSearch, enabledLocalTools, showStats)

	// Run the TUI (inline mode - no alt screen)
	p := tea.NewProgram(model)
	_, err = p.Run()

	// Cleanup MCP servers
	mcpManager.StopAll()

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
