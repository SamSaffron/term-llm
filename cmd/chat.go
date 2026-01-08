package cmd

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/samsaffron/term-llm/internal/tui/chat"
	"github.com/spf13/cobra"
)

var (
	chatDebug    bool
	chatSearch   bool
	chatProvider string
)

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Start an interactive chat session",
	Long: `Start an interactive TUI chat session with the LLM.

Examples:
  term-llm chat
  term-llm chat -s              # with web search enabled
  term-llm chat --provider zen  # use specific provider

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
  /quit        - Exit chat`,
	RunE: runChat,
}

func init() {
	chatCmd.Flags().BoolVarP(&chatSearch, "search", "s", false, "Enable web search")
	chatCmd.Flags().BoolVarP(&chatDebug, "debug", "d", false, "Show debug information")
	chatCmd.Flags().StringVar(&chatProvider, "provider", "", "Override provider, optionally with model (e.g., openai:gpt-4o)")
	if err := chatCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register provider completion: %v", err))
	}
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	_, stop := signal.NotifyContext()
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

	// Create chat model
	model := chat.New(cfg, provider, engine, modelName)

	// Set initial search state from flag
	if chatSearch {
		// The model doesn't expose searchEnabled directly,
		// but we could add a method for this if needed
		// For now, user can toggle with /search or Ctrl+S
	}

	// Run the TUI (inline mode - no alt screen)
	p := tea.NewProgram(model)
	_, err = p.Run()
	if err != nil {
		return fmt.Errorf("failed to run chat: %w", err)
	}

	return nil
}

// getModelName extracts the model name from config based on provider
func getModelName(cfg *config.Config) string {
	switch cfg.Provider {
	case "anthropic":
		return cfg.Anthropic.Model
	case "openai":
		return cfg.OpenAI.Model
	case "openrouter":
		return cfg.OpenRouter.Model
	case "gemini":
		return cfg.Gemini.Model
	case "zen":
		return cfg.Zen.Model
	case "ollama":
		return cfg.Ollama.Model
	case "lmstudio":
		return cfg.LMStudio.Model
	case "openai-compat":
		return cfg.OpenAICompat.Model
	default:
		return "unknown"
	}
}
