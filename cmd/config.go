package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
  term-llm config set providers.anthropic.model claude-opus-4-6
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

	// Get defaults
	defaults := config.GetDefaults()

	// Load config to get resolved provider configs for credential source display
	cfg, _ := config.Load()

	// Try to read raw config file and extract keys
	rawKeys := make(map[string]bool)
	unknownKeys := make(map[string]bool)
	var rawRoot yaml.Node

	data, readErr := os.ReadFile(configPath)
	if readErr == nil {
		if err := yaml.Unmarshal(data, &rawRoot); err == nil {
			extractConfigKeys(&rawRoot, "", rawKeys, unknownKeys)
		}
	}

	// Print header
	fmt.Printf("# %s\n", configPath)
	if readErr != nil {
		fmt.Printf("# (no config file - showing defaults)\n")
	}
	fmt.Println()

	// Print annotated config
	printAnnotatedConfig(defaults, rawKeys, unknownKeys, &rawRoot, readErr == nil, cfg)

	return nil
}

// extractConfigKeys walks a yaml.Node tree and extracts all key paths
// It also identifies unknown keys
func extractConfigKeys(node *yaml.Node, prefix string, rawKeys, unknownKeys map[string]bool) {
	if node == nil {
		return
	}

	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			extractConfigKeys(child, prefix, rawKeys, unknownKeys)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]

			var keyPath string
			if prefix == "" {
				keyPath = keyNode.Value
			} else {
				keyPath = prefix + "." + keyNode.Value
			}

			rawKeys[keyPath] = true

			// Check if this key is unknown
			if !config.IsKnownKey(keyPath) {
				unknownKeys[keyPath] = true
			}

			// Recurse into nested mappings
			if valueNode.Kind == yaml.MappingNode {
				extractConfigKeys(valueNode, keyPath, rawKeys, unknownKeys)
			}
		}
	}
}

// printAnnotatedConfig outputs the effective config with annotations
func printAnnotatedConfig(defaults map[string]any, rawKeys, unknownKeys map[string]bool, rawRoot *yaml.Node, hasFile bool, cfg *config.Config) {
	// Define the order and structure of sections to print
	sections := []struct {
		name   string
		keys   []string
		nested map[string][]string
	}{
		{
			name: "",
			keys: []string{"default_provider"},
		},
		{
			name: "exec",
			keys: []string{"provider", "model", "suggestions", "instructions"},
		},
		{
			name: "ask",
			keys: []string{"provider", "model", "instructions", "max_turns"},
		},
		{
			name: "chat",
			keys: []string{"provider", "model", "instructions", "max_turns"},
		},
		{
			name: "edit",
			keys: []string{"provider", "model", "instructions", "show_line_numbers", "context_lines", "editor", "diff_format"},
		},
		{
			name: "image",
			keys: []string{"provider", "output_dir"},
			nested: map[string][]string{
				"gemini":     {"model", "api_key"},
				"openai":     {"model", "api_key"},
				"xai":        {"model", "api_key"},
				"flux":       {"model", "api_key"},
				"openrouter": {"model", "api_key"},
			},
		},
		{
			name: "search",
			keys: []string{"provider", "force_external"},
			nested: map[string][]string{
				"exa":    {"api_key"},
				"brave":  {"api_key"},
				"google": {"api_key", "cx"},
			},
		},
		{
			name: "theme",
			keys: []string{"primary", "secondary", "success", "error", "warning", "muted", "text", "spinner"},
		},
		{
			name: "tools",
			keys: []string{"enabled", "read_dirs", "write_dirs", "shell_allow", "shell_auto_run", "shell_auto_run_env", "shell_non_tty_env", "image_provider"},
		},
		{
			name: "agents",
			keys: []string{"use_builtin", "search_paths"},
		},
		{
			name: "skills",
			keys: []string{"enabled", "include_project_skills", "include_ecosystem_paths"},
		},
		{
			name: "agents_md",
			keys: []string{"enabled"},
		},
		{
			name: "sessions",
			keys: []string{"enabled", "max_age_days", "max_count", "path"},
		},
		{
			name: "diagnostics",
			keys: []string{"enabled", "dir"},
		},
		{
			name: "debug_logs",
			keys: []string{"enabled", "dir"},
		},
	}

	// Get raw values from the config file for comparison
	rawValues := make(map[string]string)
	if hasFile && rawRoot != nil {
		extractRawValues(rawRoot, "", rawValues)
	}

	// Print unknown keys first as warnings
	if len(unknownKeys) > 0 {
		fmt.Println("# Unknown keys (will be ignored):")
		for key := range unknownKeys {
			if val, ok := rawValues[key]; ok {
				fmt.Printf("# %s: %s\n", key, val)
			} else {
				fmt.Printf("# %s\n", key)
			}
		}
		fmt.Println()
	}

	// Print providers section specially (dynamic keys)
	printProvidersSection(defaults, rawKeys, rawValues, hasFile, cfg)

	// Print each section
	for _, section := range sections {
		if section.name == "" {
			// Top-level keys
			for _, key := range section.keys {
				printConfigValue(key, defaults, rawKeys, rawValues, hasFile, 0)
			}
			fmt.Println()
		} else {
			// Check if section has any explicit values or if we should show defaults
			sectionHasValues := false
			for _, key := range section.keys {
				fullKey := section.name + "." + key
				if rawKeys[fullKey] || defaults[fullKey] != nil {
					sectionHasValues = true
					break
				}
			}
			for nestedSection := range section.nested {
				for _, key := range section.nested[nestedSection] {
					fullKey := section.name + "." + nestedSection + "." + key
					if rawKeys[fullKey] || defaults[fullKey] != nil {
						sectionHasValues = true
						break
					}
				}
			}

			if sectionHasValues {
				fmt.Printf("%s:\n", section.name)
				for _, key := range section.keys {
					fullKey := section.name + "." + key
					printConfigValue(fullKey, defaults, rawKeys, rawValues, hasFile, 1)
				}

				// Print nested sections
				for nestedSection, nestedKeys := range section.nested {
					nestedHasValues := false
					for _, key := range nestedKeys {
						fullKey := section.name + "." + nestedSection + "." + key
						if rawKeys[fullKey] || defaults[fullKey] != nil {
							nestedHasValues = true
							break
						}
					}
					if nestedHasValues {
						fmt.Printf("  %s:\n", nestedSection)
						for _, key := range nestedKeys {
							fullKey := section.name + "." + nestedSection + "." + key
							printConfigValue(fullKey, defaults, rawKeys, rawValues, hasFile, 2)
						}
					}
				}
				fmt.Println()
			}
		}
	}
}

// extractRawValues extracts scalar values from the YAML tree
func extractRawValues(node *yaml.Node, prefix string, values map[string]string) {
	if node == nil {
		return
	}

	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			extractRawValues(child, prefix, values)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]

			var keyPath string
			if prefix == "" {
				keyPath = keyNode.Value
			} else {
				keyPath = prefix + "." + keyNode.Value
			}

			switch valueNode.Kind {
			case yaml.ScalarNode:
				values[keyPath] = valueNode.Value
			case yaml.MappingNode:
				extractRawValues(valueNode, keyPath, values)
			case yaml.SequenceNode:
				// Format sequence as inline array with proper quoting
				var items []string
				for _, item := range valueNode.Content {
					if item.Kind == yaml.ScalarNode {
						items = append(items, quoteArrayItem(item.Value))
					}
				}
				values[keyPath] = "[" + strings.Join(items, ", ") + "]"
			}
		}
	}
}

// printProvidersSection prints the providers section with annotations
func printProvidersSection(defaults map[string]any, rawKeys map[string]bool, rawValues map[string]string, hasFile bool, cfg *config.Config) {
	// Collect all provider names from defaults and raw config
	providerNames := make(map[string]bool)

	// Default providers
	defaultProviders := []string{"anthropic", "openai", "xai", "openrouter", "gemini", "zen"}
	for _, p := range defaultProviders {
		providerNames[p] = true
	}

	// Providers from raw config
	for key := range rawKeys {
		if strings.HasPrefix(key, "providers.") {
			parts := strings.SplitN(key, ".", 3)
			if len(parts) >= 2 {
				providerNames[parts[1]] = true
			}
		}
	}

	fmt.Println("providers:")

	// Print in a consistent order (defaults first, then custom)
	for _, pName := range defaultProviders {
		printProviderConfig(pName, defaults, rawKeys, rawValues, hasFile, cfg)
	}

	// Print any custom providers
	for pName := range providerNames {
		isDefault := false
		for _, dp := range defaultProviders {
			if pName == dp {
				isDefault = true
				break
			}
		}
		if !isDefault {
			printProviderConfig(pName, defaults, rawKeys, rawValues, hasFile, cfg)
		}
	}

	fmt.Println()
}

// printProviderConfig prints a single provider's config
func printProviderConfig(name string, defaults map[string]any, rawKeys map[string]bool, rawValues map[string]string, hasFile bool, cfg *config.Config) {
	providerKeys := []string{"type", "model", "api_key", "credentials", "base_url", "url", "app_url", "app_title", "use_native_search", "models"}

	// Check if provider has any values
	hasValues := false
	for _, key := range providerKeys {
		fullKey := "providers." + name + "." + key
		if rawKeys[fullKey] || defaults[fullKey] != nil {
			hasValues = true
			break
		}
	}

	if !hasValues {
		return
	}

	fmt.Printf("  %s:\n", name)
	for _, key := range providerKeys {
		fullKey := "providers." + name + "." + key
		printConfigValue(fullKey, defaults, rawKeys, rawValues, hasFile, 2)
	}

	// Show resolved credential source
	if cfg != nil {
		var providerCfg config.ProviderConfig
		if pc, ok := cfg.Providers[name]; ok {
			providerCfg = pc
		}
		source, found := config.DescribeCredentialSource(name, &providerCfg)
		status := "✓"
		if !found {
			status = "✗"
		}
		fmt.Printf("    # credential: %s %s\n", status, source)
	}
}

// printConfigValue prints a single config value with annotation
func printConfigValue(fullKey string, defaults map[string]any, rawKeys map[string]bool, rawValues map[string]string, hasFile bool, indent int) {
	// Get the key name (last part of the path)
	parts := strings.Split(fullKey, ".")
	keyName := parts[len(parts)-1]

	defaultVal := defaults[fullKey]
	rawVal, hasRawVal := rawValues[fullKey]
	isExplicit := rawKeys[fullKey]

	// Determine what value to show
	var valueStr string
	var annotation string

	if isExplicit && hasRawVal {
		// Value was explicitly set in config
		valueStr = rawVal
		// Check if it matches default
		if defaultVal != nil && valueMatchesDefault(rawVal, defaultVal) {
			annotation = "# (same as default)"
		}
	} else if defaultVal != nil {
		// Show default value
		valueStr = formatDefaultValue(defaultVal)
		annotation = "# (default)"
	} else {
		// No default and not set - skip
		return
	}

	// Print with proper indentation
	indentStr := strings.Repeat("  ", indent)

	// Check if value is multiline - use block scalar style
	if strings.Contains(valueStr, "\n") {
		if annotation != "" {
			fmt.Printf("%s%s: |  %s\n", indentStr, keyName, annotation)
		} else {
			fmt.Printf("%s%s: |\n", indentStr, keyName)
		}
		// Print each line with extra indentation
		blockIndent := indentStr + "  "
		for _, line := range strings.Split(valueStr, "\n") {
			fmt.Printf("%s%s\n", blockIndent, line)
		}
		return
	}

	// Format single-line value
	formattedVal := formatValue(valueStr, defaultVal)
	if annotation != "" {
		fmt.Printf("%s%s: %s  %s\n", indentStr, keyName, formattedVal, annotation)
	} else {
		fmt.Printf("%s%s: %s\n", indentStr, keyName, formattedVal)
	}
}

// formatValue formats a raw value for display
func formatValue(raw string, defaultVal any) string {
	// If it looks like a sequence marker, return as-is
	if strings.HasPrefix(raw, "[") {
		return raw
	}
	// Handle booleans
	if raw == "true" || raw == "false" {
		return raw
	}
	// Handle numbers
	if _, err := fmt.Sscanf(raw, "%d", new(int)); err == nil {
		return raw
	}
	if _, err := fmt.Sscanf(raw, "%f", new(float64)); err == nil {
		return raw
	}
	// String value - check if it needs quoting
	if needsQuoting(raw) {
		return fmt.Sprintf("%q", raw)
	}
	return raw
}

// formatDefaultValue formats a default value for display
func formatDefaultValue(val any) string {
	switch v := val.(type) {
	case string:
		if needsQuoting(v) {
			return fmt.Sprintf("%q", v)
		}
		return v
	case bool:
		return fmt.Sprintf("%t", v)
	case int:
		return fmt.Sprintf("%d", v)
	case []string:
		if len(v) == 0 {
			return "[]"
		}
		var items []string
		for _, item := range v {
			items = append(items, quoteArrayItem(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// needsQuoting checks if a string value needs YAML quoting
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	// Already quoted
	if strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
		return false
	}
	if strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") {
		return false
	}
	// Check for special YAML characters that require quoting
	// Note: colon followed by space is special, but colon in URLs is fine
	if strings.Contains(s, ": ") || strings.Contains(s, "#") {
		return true
	}
	// Characters that always need quoting
	special := []string{"{", "}", "[", "]", "&", "*", "!", "|", ">", "'", "\"", "%", "@", "`"}
	for _, sp := range special {
		if strings.Contains(s, sp) {
			return true
		}
	}
	// Check if it starts with special characters
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "?") || strings.HasPrefix(s, ":") {
		return true
	}
	return false
}

// quoteArrayItem quotes an array item if it contains spaces, commas, or special chars
func quoteArrayItem(s string) string {
	// Empty string needs quoting
	if s == "" {
		return `""`
	}
	// Already quoted
	if strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
		return s
	}
	// Check if quoting is needed for inline array format
	// Spaces and commas are delimiters, special YAML chars also need quoting
	if strings.ContainsAny(s, " ,") || needsQuoting(s) {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// valueMatchesDefault checks if a raw value matches the default
func valueMatchesDefault(raw string, defaultVal any) bool {
	switch v := defaultVal.(type) {
	case string:
		return raw == v
	case bool:
		return raw == fmt.Sprintf("%t", v)
	case int:
		return raw == fmt.Sprintf("%d", v)
	case []string:
		// Compare array representation
		if len(v) == 0 {
			return raw == "[]" || raw == ""
		}
		expected := "[" + strings.Join(v, ", ") + "]"
		return raw == expected
	}
	return false
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
    model: claude-sonnet-4-6
    # Credential resolution order:
    #   1. api_key in config (or ${ANTHROPIC_API_KEY} expansion)
    #   2. ANTHROPIC_API_KEY environment variable
    #   3. CLAUDE_CODE_OAUTH_TOKEN environment variable (OAuth)
    #   4. Saved OAuth token (~/.config/term-llm/anthropic_oauth.json)
    #   5. Interactive prompt (runs 'claude setup-token')
    # Run 'term-llm config' to see which credential is active

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
    model: minimax-m2.1-free
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

# Session storage (enabled by default)
# sessions:
#   enabled: true       # Master switch - set to false to disable storage
#   max_age_days: 0     # Auto-delete sessions older than N days (0=never)
#   max_count: 0        # Keep at most N sessions (0=unlimited)
#   path: ""            # Optional DB path override (supports :memory:)

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

	// Start with keys from GetDefaults() - single source of truth
	defaults := config.GetDefaults()
	keySet := make(map[string]bool)
	for key := range defaults {
		keySet[key] = true
	}

	// Add keys that don't have defaults but are valid config keys
	extraKeys := []string{
		// Command overrides (provider/model/instructions)
		"exec.provider",
		"exec.model",
		"ask.provider",
		"ask.model",
		"ask.instructions",
		"chat.provider",
		"chat.model",
		"chat.instructions",
		"edit.provider",
		"edit.model",
		"edit.instructions",
		"edit.editor",
		// Theme
		"theme.primary",
		"theme.secondary",
		"theme.success",
		"theme.error",
		"theme.warning",
		"theme.muted",
		"theme.text",
		"theme.spinner",
		// Debug/diagnostics
		"debug_logs.enabled",
		"debug_logs.dir",
		"diagnostics.enabled",
		"diagnostics.dir",
		// Image API keys
		"image.gemini.api_key",
		"image.openai.api_key",
		"image.xai.api_key",
		"image.flux.api_key",
		"image.openrouter.api_key",
		// Search API keys
		"search.exa.api_key",
		"search.brave.api_key",
		"search.google.api_key",
		"search.google.cx",
		// Tools
		"tools.image_provider",
	}
	for _, key := range extraKeys {
		keySet[key] = true
	}

	// Add provider-specific keys
	providerNames := llm.GetProviderNames(cfg)
	for _, name := range providerNames {
		keySet["providers."+name+".model"] = true
		keySet["providers."+name+".credentials"] = true
		keySet["providers."+name+".api_key"] = true
	}

	// Convert to sorted slice
	var keys []string
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Strings(keys)

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
