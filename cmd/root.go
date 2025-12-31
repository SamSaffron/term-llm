package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var configFlag string
var printOnly bool

var rootCmd = &cobra.Command{
	Use:   "term-llm [request]",
	Short: "Translate natural language to CLI commands",
	Long: `term-llm uses AI to suggest shell commands based on your description.

Examples:
  term-llm "find all go files modified today"
  term-llm "compress this folder into a tar.gz"
  term-llm "show disk usage sorted by size"
  term-llm --config show
  term-llm --config edit`,
	Args: cobra.ArbitraryArgs,
	RunE: run,
}

func init() {
	rootCmd.Flags().StringVar(&configFlag, "config", "", "Config operation: 'show' or 'edit'")
	rootCmd.Flags().BoolVarP(&printOnly, "print-only", "p", false, "Print selected command instead of executing")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	// Handle config operations
	if configFlag != "" {
		return handleConfig(configFlag)
	}

	if len(args) == 0 {
		return fmt.Errorf("please provide a request, e.g.: term-llm \"find large files\"")
	}

	userInput := strings.Join(args, " ")
	ctx := context.Background()

	// Check if setup is needed
	var cfg *config.Config
	var err error

	if config.NeedsSetup() {
		cfg, err = ui.RunSetupWizard()
		if err != nil {
			return fmt.Errorf("setup cancelled: %w", err)
		}
	} else {
		cfg, err = config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
	}

	// Create LLM provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}

	// Detect shell
	shell := detectShell()

	// Main loop for refinement
	for {
		// Get suggestions from LLM
		suggestions, err := provider.SuggestCommands(ctx, userInput, shell, cfg.SystemContext)
		if err != nil {
			return fmt.Errorf("failed to get suggestions: %w", err)
		}

		// Sort by likelihood (highest first)
		sort.Slice(suggestions, func(i, j int) bool {
			return suggestions[i].Likelihood > suggestions[j].Likelihood
		})

		// Show selection UI
		selected, err := ui.SelectCommand(suggestions)
		if err != nil {
			return fmt.Errorf("selection cancelled: %w", err)
		}

		// Handle "something else" option
		if selected == ui.SomethingElse {
			refinement, err := ui.GetRefinement()
			if err != nil {
				return fmt.Errorf("refinement cancelled: %w", err)
			}
			if refinement == "" {
				continue
			}
			// Append refinement to original request
			userInput = fmt.Sprintf("%s (%s)", userInput, refinement)
			continue
		}

		// Print-only mode: just output the command for shell integration
		if printOnly {
			if selected != "" {
				fmt.Println(selected)
			}
			return nil
		}

		// Execute the selected command
		return executeCommand(selected, shell)
	}
}

func detectShell() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return "bash"
	}
	// Extract shell name from path (e.g., /bin/zsh -> zsh)
	parts := strings.Split(shell, "/")
	return parts[len(parts)-1]
}

func executeCommand(command, shell string) error {
	ui.ShowCommand(command)

	cmd := exec.Command(shell, "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}

	return nil
}

func handleConfig(operation string) error {
	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	switch operation {
	case "show":
		return showConfig(configPath)
	case "edit":
		return editConfig(configPath)
	default:
		return fmt.Errorf("unknown config operation: %s (use 'show' or 'edit')", operation)
	}
}

func showConfig(configPath string) error {
	// Always show effective config from defaults + env + file
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Check if file exists
	if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
		fmt.Printf("# No config file (using defaults)\n")
		fmt.Printf("# Create one at: %s\n\n", configPath)
	} else {
		fmt.Printf("# %s\n\n", configPath)
	}

	fmt.Printf("provider: %s\n\n", cfg.Provider)
	fmt.Printf("anthropic:\n")
	fmt.Printf("  model: %s\n", cfg.Anthropic.Model)
	if cfg.Anthropic.APIKey != "" {
		fmt.Printf("  api_key: [set via ANTHROPIC_API_KEY]\n")
	} else {
		fmt.Printf("  api_key: [NOT SET - export ANTHROPIC_API_KEY]\n")
	}
	fmt.Printf("\nopenai:\n")
	fmt.Printf("  model: %s\n", cfg.OpenAI.Model)
	if cfg.OpenAI.APIKey != "" {
		fmt.Printf("  api_key: [set via OPENAI_API_KEY]\n")
	} else {
		fmt.Printf("  api_key: [NOT SET - export OPENAI_API_KEY]\n")
	}
	return nil
}

func editConfig(configPath string) error {
	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Create default config if it doesn't exist
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, []byte(defaultConfig()), 0644); err != nil {
			return fmt.Errorf("failed to create config file: %w", err)
		}
	}

	// Get editor from environment
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}

	cmd := exec.Command(editor, configPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultConfig() string {
	return `provider: anthropic  # or "openai"

# API keys are read from environment variables:
#   ANTHROPIC_API_KEY for Anthropic
#   OPENAI_API_KEY for OpenAI

# Custom context added to system prompt
system_context: |


anthropic:
  model: claude-sonnet-4-5

openai:
  model: gpt-5.2
`
}
