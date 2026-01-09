package cmd

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tui/chat"
	"github.com/spf13/cobra"
)

var (
	chatDebug           bool
	chatSearch          bool
	chatProvider        string
	chatMCP             string
	chatMaxTurns        int
	chatNativeSearch    bool
	chatNoNativeSearch  bool
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
	if err := chatCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register provider completion: %v", err))
	}
	if err := chatCmd.RegisterFlagCompletionFunc("mcp", MCPFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register mcp completion: %v", err))
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

	if err := applyProviderOverrides(cfg, cfg.Ask.Provider, cfg.Ask.Model, chatProvider); err != nil {
		return err
	}

	initThemeFromConfig(cfg)

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := llm.NewEngine(provider, defaultToolRegistry())

	// Determine model name
	modelName := getModelName(cfg)

	// Create MCP manager
	mcpManager := mcp.NewManager()
	if err := mcpManager.LoadConfig(); err != nil {
		// Non-fatal: continue without MCP
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to load MCP config: %v\n", err)
	}

	// Enable MCP servers from --mcp flag
	if chatMCP != "" {
		servers := strings.Split(chatMCP, ",")
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
	model := chat.New(cfg, provider, engine, modelName, mcpManager, chatMaxTurns, forceExternalSearch)

	// Set initial search state from flag
	if chatSearch {
		// The model doesn't expose searchEnabled directly,
		// but we could add a method for this if needed
		// For now, user can toggle with /search or Ctrl+S
	}

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
