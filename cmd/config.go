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
  term-llm config reset               # reset to defaults
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

var configResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset configuration to defaults",
	Long:  `Reset the configuration file to default values. This will overwrite any existing configuration.`,
	RunE:  configReset,
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configEditCmd)
	configCmd.AddCommand(configPathCmd)
	configCmd.AddCommand(configCompletionCmd)
	configCmd.AddCommand(configResetCmd)
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
	printCredentialStatus("anthropic", cfg.Anthropic.Credentials, cfg.Anthropic.APIKey, "ANTHROPIC_API_KEY")

	fmt.Printf("\nopenai:\n")
	fmt.Printf("  model: %s\n", cfg.OpenAI.Model)
	printCredentialStatus("openai", cfg.OpenAI.Credentials, cfg.OpenAI.APIKey, "OPENAI_API_KEY")

	fmt.Printf("\ngemini:\n")
	fmt.Printf("  model: %s\n", cfg.Gemini.Model)
	// For gemini-cli, check OAuthCreds instead of APIKey
	geminiKey := cfg.Gemini.APIKey
	if cfg.Gemini.Credentials == "gemini-cli" && cfg.Gemini.OAuthCreds != nil {
		geminiKey = "oauth" // non-empty to indicate success
	}
	printCredentialStatus("gemini", cfg.Gemini.Credentials, geminiKey, "GEMINI_API_KEY")

	fmt.Printf("\nzen:\n")
	fmt.Printf("  model: %s\n", cfg.Zen.Model)
	printZenCredentialStatus(cfg.Zen.APIKey)

	return nil
}

func printCredentialStatus(provider, credType, apiKey, envVar string) {
	switch credType {
	case "claude":
		if apiKey != "" {
			fmt.Printf("  credentials: claude [OK]\n")
		} else {
			fmt.Printf("  credentials: claude [FAILED - run 'claude' to login]\n")
		}
	case "codex":
		if apiKey != "" {
			fmt.Printf("  credentials: codex [OK]\n")
		} else {
			fmt.Printf("  credentials: codex [FAILED - run 'codex login']\n")
		}
	case "gemini-cli":
		if apiKey != "" {
			fmt.Printf("  credentials: gemini-cli [OK]\n")
		} else {
			fmt.Printf("  credentials: gemini-cli [FAILED - run 'gemini' to configure]\n")
		}
	default:
		if apiKey != "" {
			fmt.Printf("  credentials: api_key [set via %s]\n", envVar)
		} else {
			fmt.Printf("  credentials: api_key [NOT SET - export %s]\n", envVar)
		}
	}
}

// printZenCredentialStatus shows the OpenCode Zen credential status
// Zen has free tier access, so empty API key is valid
func printZenCredentialStatus(apiKey string) {
	if apiKey != "" {
		fmt.Printf("  credentials: api_key [set via ZEN_API_KEY]\n")
	} else {
		fmt.Printf("  credentials: [free tier - no API key required]\n")
	}
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

func configReset(cmd *cobra.Command, args []string) error {
	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Write default config
	if err := os.WriteFile(configPath, []byte(defaultConfigContent()), 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	fmt.Printf("Config reset to defaults: %s\n", configPath)
	return nil
}

func defaultConfigContent() string {
	return `# term-llm configuration
# Run 'term-llm config edit' to modify

provider: anthropic  # anthropic, openai, gemini, or zen

# exec command settings
exec:
  suggestions: 3  # number of command suggestions to show
  # instructions: |
  #   Custom context for command suggestions, e.g.:
  #   - I use macOS with zsh
  #   - Prefer ripgrep over grep, fd over find
  #   - Always use --color=auto for grep

# ask command settings
ask:
  # instructions: |
  #   Custom system prompt for ask command, e.g.:
  #   - Be concise, I'm an experienced developer
  #   - Prefer practical examples over theory

# UI theme colors (ANSI 0-255 or hex #RRGGBB)
# theme:
#   primary: "10"     # commands, highlights (default: bright green)
#   muted: "245"      # explanations, footers (default: light grey)
#   spinner: "205"    # loading spinner (default: pink)
#   error: "9"        # error messages (default: bright red)

# Provider configurations
anthropic:
  model: claude-sonnet-4-5
  # credentials: api_key or claude
  #   api_key: uses ANTHROPIC_API_KEY env var (default)
  #   claude: uses Claude Code credentials (requires 'claude' CLI)

openai:
  model: gpt-5.2
  # credentials: api_key or codex
  #   api_key: uses OPENAI_API_KEY env var (default)
  #   codex: uses Codex credentials (requires 'codex' CLI)

gemini:
  model: gemini-3-flash-preview
  # credentials: api_key or gemini-cli
  #   api_key: uses GEMINI_API_KEY env var (default)
  #   gemini-cli: uses gemini-cli OAuth (requires 'gemini' CLI)

zen:
  model: glm-4.7-free
  # OpenCode Zen - free tier access to GLM 4.7 and other models
  # api_key is optional - leave empty for free tier access
  # Set ZEN_API_KEY env var if you have a paid API key
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
