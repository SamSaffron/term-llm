package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
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

var configEditMcpCmd = &cobra.Command{
	Use:   "edit-mcp",
	Short: "Edit MCP configuration file in $EDITOR",
	RunE:  configEditMcp,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Long: `Set a configuration value while preserving comments.

Examples:
  term-llm config set default_provider openai
  term-llm config set default_provider gemini
  term-llm config set providers.anthropic.model claude-opus-4-5
  term-llm config set exec.suggestions 5
  term-llm config set image.provider flux`,
	Args:              cobra.ExactArgs(2),
	RunE:              configSet,
	ValidArgsFunction: configSetCompletion,
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a configuration value",
	Long: `Get a configuration value.

Examples:
  term-llm config get default_provider
  term-llm config get providers.anthropic.model`,
	Args:              cobra.ExactArgs(1),
	RunE:              configGet,
	ValidArgsFunction: configGetCompletion,
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configEditCmd)
	configCmd.AddCommand(configPathCmd)
	configCmd.AddCommand(configCompletionCmd)
	configCmd.AddCommand(configResetCmd)
	configCmd.AddCommand(configEditMcpCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
}

func configShow(cmd *cobra.Command, args []string) error {
	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	// If config file exists, just print it as-is (preserves comments, all keys)
	if data, err := os.ReadFile(configPath); err == nil {
		fmt.Printf("# %s\n\n", configPath)
		os.Stdout.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			fmt.Println()
		}
		return nil
	}

	// No config file - show helpful message with defaults
	fmt.Printf("# No config file (using defaults)\n")
	fmt.Printf("# Create one with: term-llm config reset\n")
	fmt.Printf("# Or edit directly: term-llm config edit\n")
	fmt.Printf("# Config path: %s\n", configPath)
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

func configEditMcp(cmd *cobra.Command, args []string) error {
	mcpPath, err := mcp.DefaultConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get MCP config path: %w", err)
	}

	// Ensure config directory exists
	mcpDir := filepath.Dir(mcpPath)
	if err := os.MkdirAll(mcpDir, 0755); err != nil {
		return fmt.Errorf("failed to create MCP config directory: %w", err)
	}

	// Create default config if it doesn't exist
	if _, err := os.Stat(mcpPath); os.IsNotExist(err) {
		defaultCfg := &mcp.Config{Servers: make(map[string]mcp.ServerConfig)}
		if err := defaultCfg.SaveToPath(mcpPath); err != nil {
			return fmt.Errorf("failed to create MCP config file: %w", err)
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

	editorCmd := exec.Command(editor, mcpPath)
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

default_provider: anthropic

providers:
  # Built-in providers - type is inferred from the key name
  anthropic:
    model: claude-sonnet-4-5
    # credentials: api_key (default)

  openai:
    model: gpt-5.2

  gemini:
    model: gemini-3-flash-preview
    # credentials: api_key (default) or gemini-cli (gemini-cli OAuth)

  openrouter:
    model: x-ai/grok-code-fast-1
    app_url: https://github.com/samsaffron/term-llm
    app_title: term-llm

  zen:
    model: glm-4.7-free
    # api_key optional - free tier access via opencode.ai

  # Local LLM providers (require explicit type)
  # ollama:
  #   type: openai_compatible
  #   base_url: http://localhost:11434/v1
  #   model: llama3.2:latest

  # lmstudio:
  #   type: openai_compatible
  #   base_url: http://localhost:1234/v1
  #   model: deepseek-coder-v2

  # Custom OpenAI-compatible endpoints
  # cerebras:
  #   type: openai_compatible
  #   base_url: https://api.cerebras.ai/v1
  #   model: llama-4-scout-17b
  #   api_key: ${CEREBRAS_API_KEY}

  # groq:
  #   type: openai_compatible
  #   base_url: https://api.groq.com/openai/v1
  #   model: llama-3.3-70b-versatile
  #   api_key: ${GROQ_API_KEY}

# Per-command overrides
exec:
  suggestions: 3
  # provider: anthropic    # override provider for exec only
  # model: claude-opus-4   # override model for exec only
  # instructions: |
  #   Custom context for command suggestions

ask:
  # provider: openai       # override provider for ask only
  # model: gpt-4o

edit:
  # provider: anthropic    # override provider for edit only

# UI theme colors (ANSI 0-255 or hex #RRGGBB)
# theme:
#   primary: "10"     # commands, highlights
#   muted: "245"      # explanations, footers
#   spinner: "205"    # loading spinner
#   error: "9"        # error messages

# Debug logging (for troubleshooting LLM requests)
# debug_logs:
#   enabled: false
#   dir: ~/.local/share/term-llm/debug/

# Image generation settings
image:
  provider: gemini  # gemini, openai, or flux
  output_dir: ~/Pictures/term-llm

  gemini:
    model: gemini-2.5-flash-image
    # api_key: uses GEMINI_API_KEY env var

  openai:
    model: gpt-image-1
    # api_key: uses OPENAI_API_KEY env var

  flux:
    model: flux-2-pro
    # api_key: uses BFL_API_KEY env var
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
		fmt.Fprintln(os.Stderr, "Then restart your shell")
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

// configSet sets a configuration value while preserving comments
func configSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Read existing file or create empty document
	var root yaml.Node
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create new document with empty mapping
			root = yaml.Node{
				Kind: yaml.DocumentNode,
				Content: []*yaml.Node{{
					Kind: yaml.MappingNode,
				}},
			}
		} else {
			return fmt.Errorf("failed to read config: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}
	}

	// Navigate/create path and set value
	keyParts := strings.Split(key, ".")
	if err := setYAMLValue(&root, keyParts, value); err != nil {
		return fmt.Errorf("failed to set value: %w", err)
	}

	// Write back
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}
	encoder.Close()

	if err := os.WriteFile(configPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	fmt.Printf("%s = %s\n", key, value)
	return nil
}

// setYAMLValue navigates/creates the path in a yaml.Node tree and sets the value
func setYAMLValue(root *yaml.Node, path []string, value string) error {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("invalid document structure")
	}

	current := root.Content[0]
	if current.Kind != yaml.MappingNode {
		return fmt.Errorf("root is not a mapping")
	}

	for i, part := range path {
		isLast := i == len(path)-1

		// Find or create the key
		found := false
		for j := 0; j < len(current.Content); j += 2 {
			keyNode := current.Content[j]
			if keyNode.Value == part {
				if isLast {
					// Set the value
					valueNode := current.Content[j+1]
					valueNode.Value = value
					valueNode.Tag = ""
					valueNode.Kind = yaml.ScalarNode
				} else {
					// Navigate deeper
					current = current.Content[j+1]
					if current.Kind != yaml.MappingNode {
						// Convert to mapping if needed
						current.Kind = yaml.MappingNode
						current.Content = nil
						current.Value = ""
						current.Tag = ""
					}
				}
				found = true
				break
			}
		}

		if !found {
			// Create the key
			keyNode := &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: part,
			}

			if isLast {
				// Create scalar value
				valueNode := &yaml.Node{
					Kind:  yaml.ScalarNode,
					Value: value,
				}
				current.Content = append(current.Content, keyNode, valueNode)
			} else {
				// Create mapping for intermediate path
				newMapping := &yaml.Node{
					Kind: yaml.MappingNode,
				}
				current.Content = append(current.Content, keyNode, newMapping)
				current = newMapping
			}
		}
	}

	return nil
}

// configGet gets a configuration value
func configGet(cmd *cobra.Command, args []string) error {
	key := args[0]

	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("config file does not exist")
		}
		return fmt.Errorf("failed to read config: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	value, err := getYAMLValue(&root, strings.Split(key, "."))
	if err != nil {
		return err
	}

	fmt.Println(value)
	return nil
}

// getYAMLValue navigates the yaml.Node tree and returns the value at path
func getYAMLValue(root *yaml.Node, path []string) (string, error) {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return "", fmt.Errorf("invalid document structure")
	}

	current := root.Content[0]
	for _, part := range path {
		if current.Kind != yaml.MappingNode {
			return "", fmt.Errorf("path not found: expected mapping")
		}

		found := false
		for j := 0; j < len(current.Content); j += 2 {
			if current.Content[j].Value == part {
				current = current.Content[j+1]
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("key not found: %s", part)
		}
	}

	if current.Kind == yaml.ScalarNode {
		return current.Value, nil
	}
	return "", fmt.Errorf("value is not a scalar")
}

// configSetCompletion provides completions for config set
func configSetCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		// Complete config keys
		return configKeyCompletions(toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	if len(args) == 1 {
		// Complete values based on the key
		return configValueCompletions(args[0], toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// configGetCompletion provides completions for config get
func configGetCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return configKeyCompletions(toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// configKeyCompletions returns completions for config keys
func configKeyCompletions(toComplete string) []string {
	// Load config to get dynamic provider names
	cfg, _ := config.Load()

	keys := []string{
		"default_provider",
		"exec.provider",
		"exec.model",
		"exec.suggestions",
		"exec.instructions",
		"ask.provider",
		"ask.model",
		"ask.instructions",
		"ask.max_turns",
		"edit.provider",
		"edit.model",
		"edit.instructions",
		"edit.show_line_numbers",
		"edit.context_lines",
		"edit.diff_format",
		"image.provider",
		"image.output_dir",
		"image.gemini.model",
		"image.openai.model",
		"image.flux.model",
		"search.provider",
		"search.force_external",
		"theme.primary",
		"theme.secondary",
		"theme.success",
		"theme.error",
		"theme.warning",
		"theme.muted",
		"theme.spinner",
	}

	// Add provider-specific keys
	providerNames := llm.GetProviderNames(cfg)
	for _, name := range providerNames {
		keys = append(keys, "providers."+name+".model")
		keys = append(keys, "providers."+name+".credentials")
	}

	var completions []string
	for _, key := range keys {
		if strings.HasPrefix(key, toComplete) {
			completions = append(completions, key)
		}
	}
	return completions
}

// configValueCompletions returns completions for config values based on key
func configValueCompletions(key, toComplete string) []string {
	cfg, _ := config.Load()

	switch key {
	case "default_provider", "exec.provider", "ask.provider", "edit.provider":
		// Provider names
		names := llm.GetProviderNames(cfg)
		var completions []string
		for _, name := range names {
			if strings.HasPrefix(name, toComplete) {
				completions = append(completions, name)
			}
		}
		return completions

	case "image.provider":
		providers := []string{"gemini", "openai", "flux"}
		var completions []string
		for _, p := range providers {
			if strings.HasPrefix(p, toComplete) {
				completions = append(completions, p)
			}
		}
		return completions

	case "search.provider":
		providers := []string{"duckduckgo", "exa", "brave", "google"}
		var completions []string
		for _, p := range providers {
			if strings.HasPrefix(p, toComplete) {
				completions = append(completions, p)
			}
		}
		return completions

	case "edit.diff_format":
		formats := []string{"auto", "udiff", "replace"}
		var completions []string
		for _, f := range formats {
			if strings.HasPrefix(f, toComplete) {
				completions = append(completions, f)
			}
		}
		return completions

	case "edit.show_line_numbers", "search.force_external":
		bools := []string{"true", "false"}
		var completions []string
		for _, b := range bools {
			if strings.HasPrefix(b, toComplete) {
				completions = append(completions, b)
			}
		}
		return completions
	}

	// Check for provider model keys
	if strings.HasPrefix(key, "providers.") && strings.HasSuffix(key, ".model") {
		parts := strings.Split(key, ".")
		if len(parts) == 3 {
			provider := parts[1]
			models := llm.ProviderModels[provider]
			if models == nil {
				// Try inferring type
				providerType := string(config.InferProviderType(provider, ""))
				models = llm.ProviderModels[providerType]
			}
			var completions []string
			for _, m := range models {
				if strings.HasPrefix(m, toComplete) {
					completions = append(completions, m)
				}
			}
			return completions
		}
	}

	// Check for provider credentials keys
	if strings.HasPrefix(key, "providers.") && strings.HasSuffix(key, ".credentials") {
		parts := strings.Split(key, ".")
		if len(parts) == 3 {
			provider := parts[1]
			providerType := config.InferProviderType(provider, "")
			var creds []string
			switch providerType {
			case config.ProviderTypeAnthropic:
				creds = []string{"api_key", "claude"}
			case config.ProviderTypeGemini:
				creds = []string{"api_key", "gemini-cli"}
			default:
				creds = []string{"api_key"}
			}
			var completions []string
			for _, c := range creds {
				if strings.HasPrefix(c, toComplete) {
					completions = append(completions, c)
				}
			}
			return completions
		}
	}

	// Image model completions
	if key == "image.gemini.model" {
		return filterPrefix(llm.ImageProviderModels["gemini"], toComplete)
	}
	if key == "image.openai.model" {
		return filterPrefix(llm.ImageProviderModels["openai"], toComplete)
	}
	if key == "image.flux.model" {
		return filterPrefix(llm.ImageProviderModels["flux"], toComplete)
	}

	return nil
}

// filterPrefix filters a slice to items starting with prefix
func filterPrefix(items []string, prefix string) []string {
	var result []string
	for _, item := range items {
		if strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return result
}
