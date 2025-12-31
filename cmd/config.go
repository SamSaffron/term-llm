package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage term-llm configuration",
	Long: `View or edit your term-llm configuration.

Examples:
  term-llm config                     # show current config
  term-llm config edit                # edit in $EDITOR
  term-llm config completion zsh      # generate shell completions`,
	RunE: configShow, // Default to show
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Edit configuration file in $EDITOR",
	RunE:  configEdit,
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print configuration file path",
	RunE:  configPath,
}

var configCompletionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate shell completion script",
	Long: `Generate shell completion script and print setup instructions.

Examples:
  term-llm config completion bash
  term-llm config completion zsh
  term-llm config completion fish`,
	ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE:      configCompletion,
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configEditCmd)
	configCmd.AddCommand(configPathCmd)
	configCmd.AddCommand(configCompletionCmd)
}

func configShow(cmd *cobra.Command, args []string) error {
	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

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

func configEdit(cmd *cobra.Command, args []string) error {
	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Create default config if it doesn't exist
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, []byte(defaultConfigContent()), 0644); err != nil {
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

	editorCmd := exec.Command(editor, configPath)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	return editorCmd.Run()
}

func configPath(cmd *cobra.Command, args []string) error {
	path, err := config.GetConfigPath()
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func defaultConfigContent() string {
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

var installCompletions bool

func init() {
	configCompletionCmd.Flags().BoolVar(&installCompletions, "install", false, "Install completions to standard location")
}

func configCompletion(cmd *cobra.Command, args []string) error {
	shell := args[0]

	if installCompletions {
		return installShellCompletion(shell)
	}

	// Just output to stdout
	switch shell {
	case "bash":
		return rootCmd.GenBashCompletion(os.Stdout)
	case "zsh":
		return rootCmd.GenZshCompletion(os.Stdout)
	case "fish":
		return rootCmd.GenFishCompletion(os.Stdout, true)
	case "powershell":
		return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
	}
	return nil
}

func installShellCompletion(shell string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	var path string
	var content []byte
	var buf = new(bytes.Buffer)

	switch shell {
	case "bash":
		path = filepath.Join(home, ".bash_completion.d", "term-llm")
		if err := rootCmd.GenBashCompletion(buf); err != nil {
			return err
		}
		content = buf.Bytes()

	case "zsh":
		// Use ~/.local/share/zsh/site-functions which is the XDG standard
		path = filepath.Join(home, ".local", "share", "zsh", "site-functions", "_term-llm")
		if err := rootCmd.GenZshCompletion(buf); err != nil {
			return err
		}
		content = buf.Bytes()

	case "fish":
		path = filepath.Join(home, ".config", "fish", "completions", "term-llm.fish")
		if err := rootCmd.GenFishCompletion(buf, true); err != nil {
			return err
		}
		content = buf.Bytes()

	case "powershell":
		// PowerShell completions go in the profile directory
		path = filepath.Join(home, ".config", "powershell", "completions", "term-llm.ps1")
		if err := rootCmd.GenPowerShellCompletionWithDesc(buf); err != nil {
			return err
		}
		content = buf.Bytes()

	default:
		return fmt.Errorf("unknown shell: %s", shell)
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Write completion file
	if err := os.WriteFile(path, content, 0644); err != nil {
		return fmt.Errorf("failed to write completion file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Installed completions to %s\n", path)

	// Print shell-specific instructions
	switch shell {
	case "bash":
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Add to ~/.bashrc:")
		fmt.Fprintf(os.Stderr, "  source %s\n", path)
	case "zsh":
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Ensure ~/.zshrc has (before compinit):")
		fmt.Fprintf(os.Stderr, "  fpath+=(%s)\n", dir)
		fmt.Fprintln(os.Stderr, "  autoload -U compinit && compinit")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Then restart your shell or run: exec zsh")
	case "fish":
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Completions will be loaded automatically.")
		fmt.Fprintln(os.Stderr, "Restart your shell or run: exec fish")
	case "powershell":
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Add to your PowerShell profile:")
		fmt.Fprintf(os.Stderr, "  . %s\n", path)
	}

	return nil
}
