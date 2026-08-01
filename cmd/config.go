package cmd

import (
	"bytes"
	"fmt"
	"io"
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

	// Print effective approval defaults before the raw/effective config tree so
	// optional unset keys remain discoverable without materializing them.
	printEffectiveApprovalModes(os.Stdout, cfg)

	// Print annotated config
	printAnnotatedConfig(os.Stdout, defaults, rawKeys, unknownKeys, &rawRoot, readErr == nil, cfg)

	return nil
}

func printEffectiveApprovalModes(out io.Writer, cfg *config.Config) {
	if out == nil || cfg == nil {
		return
	}
	fmt.Fprintln(out, "# Effective approval modes (mode and resolution source):")
	for _, item := range []struct {
		label string
		key   string
	}{
		{"chat", "chat.approval_mode"},
		{"ask", "ask.approval_mode"},
		{"edit", "edit.approval_mode"},
		{"exec", "exec.approval_mode"},
		{"loop", "loop.approval_mode"},
		{"serve", "serve.approval_mode"},
		{"serve mcp", "serve.mcp.approval_mode"},
	} {
		if value, ok, err := effectiveApprovalConfigValue(item.key, cfg); err == nil && ok {
			fmt.Fprintf(out, "#   %s: %s\n", item.label, value)
		}
	}
	fmt.Fprintln(out)
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

// printAnnotatedConfig outputs the effective config with annotations.
func printAnnotatedConfig(out io.Writer, defaults map[string]any, rawKeys, unknownKeys map[string]bool, rawRoot *yaml.Node, hasFile bool, cfg *config.Config) {
	printAnnotatedConfigFiltered(out, defaults, rawKeys, unknownKeys, rawRoot, hasFile, cfg, false)
}

func printAnnotatedConfigFiltered(out io.Writer, defaults map[string]any, rawKeys, unknownKeys map[string]bool, rawRoot *yaml.Node, hasFile bool, cfg *config.Config, resetTemplate bool) {
	// Get raw values from the config file for comparison
	rawValues := make(map[string]string)
	if hasFile && rawRoot != nil {
		extractRawValues(rawRoot, "", rawValues)
	}

	// Print unknown keys first as warnings
	if len(unknownKeys) > 0 {
		fmt.Fprintln(out, "# Unknown keys (will be ignored):")
		keys := make([]string, 0, len(unknownKeys))
		for key := range unknownKeys {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if val, ok := rawValues[key]; ok {
				fmt.Fprintf(out, "# %s: %s\n", key, val)
			} else {
				fmt.Fprintf(out, "# %s\n", key)
			}
		}
		fmt.Fprintln(out)
	}

	// Print providers section specially (dynamic keys)
	printProvidersSection(out, defaults, rawKeys, rawValues, hasFile, cfg)

	var keys []renderKey
	for _, spec := range config.ConfigKeySpecs() {
		if !spec.ShowInConfig || (resetTemplate && !spec.ResetTemplate) {
			continue
		}
		keys = append(keys, renderKeyFromSpec(spec))
	}
	renderConfigTree(out, keys, rawKeys, rawValues, hasFile, 0)
}

type renderKey struct {
	RenderPath  string
	FullPath    string
	Default     any
	HasDefault  bool
	Sensitive   bool
	Placeholder any
}

type renderNode struct {
	name     string
	key      *renderKey
	children []*renderNode
	byName   map[string]*renderNode
}

func renderKeyFromSpec(spec config.KeySpec) renderKey {
	return renderKey{
		RenderPath:  spec.Path,
		FullPath:    spec.Path,
		Default:     spec.Default,
		HasDefault:  spec.HasDefault,
		Sensitive:   spec.Sensitive,
		Placeholder: spec.Placeholder,
	}
}

func renderKeyFromProviderSpec(providerName string, spec config.ProviderFieldSpec, defaultVal any, hasDefault bool) renderKey {
	placeholder := spec.Placeholder
	if placeholder == nil {
		placeholder = ""
	}
	return renderKey{
		RenderPath:  spec.Path,
		FullPath:    "providers." + providerName + "." + spec.Path,
		Default:     defaultVal,
		HasDefault:  hasDefault,
		Sensitive:   spec.Sensitive,
		Placeholder: placeholder,
	}
}

func renderConfigTree(out io.Writer, keys []renderKey, rawKeys map[string]bool, rawValues map[string]string, hasFile bool, indent int) {
	root := &renderNode{byName: make(map[string]*renderNode)}
	for i := range keys {
		insertRenderKey(root, &keys[i])
	}
	for _, child := range root.children {
		renderNodeTree(out, child, rawKeys, rawValues, hasFile, indent, false)
	}
	if len(root.children) > 0 {
		fmt.Fprintln(out)
	}
}

func insertRenderKey(root *renderNode, key *renderKey) {
	parts := strings.Split(key.RenderPath, ".")
	current := root
	for _, part := range parts {
		if current.byName == nil {
			current.byName = make(map[string]*renderNode)
		}
		child := current.byName[part]
		if child == nil {
			child = &renderNode{name: part, byName: make(map[string]*renderNode)}
			current.byName[part] = child
			current.children = append(current.children, child)
		}
		current = child
	}
	current.key = key
}

func renderNodeTree(out io.Writer, node *renderNode, rawKeys map[string]bool, rawValues map[string]string, hasFile bool, indent int, parentCommented bool) {
	if node == nil {
		return
	}
	if len(node.children) == 0 {
		if node.key != nil {
			printRenderValue(out, *node.key, rawKeys, rawValues, hasFile, indent, parentCommented)
		}
		return
	}

	commented := parentCommented || !renderNodeHasActive(node, rawKeys)
	indentStr := strings.Repeat("  ", indent)
	commentPrefix := ""
	if commented {
		commentPrefix = "# "
	}
	fmt.Fprintf(out, "%s%s%s:\n", indentStr, commentPrefix, node.name)
	for _, child := range node.children {
		renderNodeTree(out, child, rawKeys, rawValues, hasFile, indent+1, commented)
	}
}

func renderNodeHasActive(node *renderNode, rawKeys map[string]bool) bool {
	if node == nil {
		return false
	}
	if node.key != nil && (node.key.HasDefault || rawKeys[node.key.FullPath]) {
		return true
	}
	for _, child := range node.children {
		if renderNodeHasActive(child, rawKeys) {
			return true
		}
	}
	return false
}

func printRenderValue(out io.Writer, key renderKey, rawKeys map[string]bool, rawValues map[string]string, hasFile bool, indent int, parentCommented bool) {
	parts := strings.Split(key.RenderPath, ".")
	keyName := parts[len(parts)-1]
	rawVal, hasRawVal := rawValues[key.FullPath]
	isExplicit := rawKeys[key.FullPath]
	active := !parentCommented && (isExplicit || key.HasDefault)

	var valueStr string
	var annotation string
	if active && isExplicit && hasRawVal {
		if key.Sensitive && strings.TrimSpace(rawVal) != "" {
			valueStr = "<redacted>"
			annotation = "# (set, redacted)"
		} else {
			valueStr = rawVal
			if key.HasDefault && valueMatchesDefault(rawVal, key.Default) {
				annotation = "# (same as default)"
			} else {
				annotation = "# (set)"
			}
		}
	} else if active && key.HasDefault {
		valueStr = formatDefaultValue(key.Default)
		annotation = "# (default)"
	} else {
		valueStr = formatPlaceholderValue(key.Placeholder)
		annotation = "# (unset)"
	}

	indentStr := strings.Repeat("  ", indent)
	commentPrefix := ""
	if !active {
		commentPrefix = "# "
	}

	// Print with proper indentation. Multiline active values use block scalar style;
	// commented placeholders are intentionally single-line examples.
	if active && strings.Contains(valueStr, "\n") {
		if annotation != "" {
			fmt.Fprintf(out, "%s%s: |  %s\n", indentStr, keyName, annotation)
		} else {
			fmt.Fprintf(out, "%s%s: |\n", indentStr, keyName)
		}
		blockIndent := indentStr + "  "
		for _, line := range strings.Split(valueStr, "\n") {
			fmt.Fprintf(out, "%s%s\n", blockIndent, line)
		}
		return
	}

	formattedVal := formatValue(valueStr, key.Default)
	if annotation != "" {
		fmt.Fprintf(out, "%s%s%s: %s  %s\n", indentStr, commentPrefix, keyName, formattedVal, annotation)
	} else {
		fmt.Fprintf(out, "%s%s%s: %s\n", indentStr, commentPrefix, keyName, formattedVal)
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

// printProvidersSection prints the providers section with annotations.
func printProvidersSection(out io.Writer, defaults map[string]any, rawKeys map[string]bool, rawValues map[string]string, hasFile bool, cfg *config.Config) {
	providerNames := make(map[string]bool)
	defaultProviders := config.DefaultProviderNames()
	for _, p := range defaultProviders {
		providerNames[p] = true
	}

	// Providers from raw config.
	for key := range rawKeys {
		if strings.HasPrefix(key, "providers.") {
			parts := strings.SplitN(key, ".", 3)
			if len(parts) >= 2 {
				providerNames[parts[1]] = true
			}
		}
	}

	fmt.Fprintln(out, "providers:")

	printed := make(map[string]bool)
	for _, pName := range defaultProviders {
		printProviderConfig(out, pName, true, defaults, rawKeys, rawValues, hasFile, cfg)
		printed[pName] = true
	}

	var custom []string
	for pName := range providerNames {
		if !printed[pName] {
			custom = append(custom, pName)
		}
	}
	sort.Strings(custom)
	for _, pName := range custom {
		printProviderConfig(out, pName, false, defaults, rawKeys, rawValues, hasFile, cfg)
	}

	fmt.Fprintln(out)
}

// printProviderConfig prints a single provider's config.
func printProviderConfig(out io.Writer, name string, includeOptional bool, defaults map[string]any, rawKeys map[string]bool, rawValues map[string]string, hasFile bool, cfg *config.Config) {
	var keys []renderKey
	for _, field := range config.ProviderKeySpecs() {
		fullKey := "providers." + name + "." + field.Path
		defaultVal, hasDefault := defaults[fullKey]
		if includeOptional || hasDefault || rawKeys[fullKey] || hasRawDescendant(rawKeys, fullKey+".") {
			keys = append(keys, renderKeyFromProviderSpec(name, field, defaultVal, hasDefault))
		}
	}

	// Include dynamic env/model_map keys from the raw file so config show does not
	// hide case-sensitive env vars or custom Bedrock model aliases.
	dynamicPrefix := "providers." + name + "."
	for rawKey := range rawKeys {
		if !strings.HasPrefix(rawKey, dynamicPrefix) {
			continue
		}
		rel := strings.TrimPrefix(rawKey, dynamicPrefix)
		if strings.HasPrefix(rel, "env.") || strings.HasPrefix(rel, "model_map.") {
			keys = append(keys, renderKey{RenderPath: rel, FullPath: rawKey, Sensitive: isSensitiveProviderEnvKey(rel), Placeholder: ""})
		}
	}

	if len(keys) == 0 {
		return
	}
	root := &renderNode{byName: make(map[string]*renderNode)}
	for i := range keys {
		insertRenderKey(root, &keys[i])
	}
	if !includeOptional && !renderNodeHasActive(root, rawKeys) {
		return
	}

	fmt.Fprintf(out, "  %s:\n", name)
	for _, child := range root.children {
		renderNodeTree(out, child, rawKeys, rawValues, hasFile, 2, false)
	}

	// Show resolved credential source.
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
		fmt.Fprintf(out, "    # credential: %s %s\n", status, source)
	}
}

func hasRawDescendant(rawKeys map[string]bool, prefix string) bool {
	for key := range rawKeys {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func isSensitiveProviderEnvKey(rel string) bool {
	if !strings.HasPrefix(rel, "env.") {
		return false
	}
	name := strings.ToUpper(strings.TrimPrefix(rel, "env."))
	for _, marker := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// formatValue formats a raw value for display
func formatValue(raw string, defaultVal any) string {
	// If it looks like a sequence or map marker, return as-is
	if strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "{") {
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
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	case []string:
		if len(v) == 0 {
			return "[]"
		}
		var items []string
		for _, item := range v {
			items = append(items, quoteArrayItem(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case []int64:
		if len(v) == 0 {
			return "[]"
		}
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, fmt.Sprintf("%d", item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case map[string]any:
		if len(v) == 0 {
			return "{}"
		}
	}
	return fmt.Sprintf("%v", val)
}

func formatPlaceholderValue(val any) string {
	if val == nil {
		return "null"
	}
	return formatDefaultValue(val)
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
	case int64:
		return raw == fmt.Sprintf("%d", v)
	case float64:
		return raw == fmt.Sprintf("%g", v)
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
		if err := config.WriteFileAtomically(configPath, []byte(defaultConfigContent()), 0o600); err != nil {
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
	if err := config.WriteFileAtomically(configPath, []byte(defaultConfigContent()), 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	fmt.Printf("Config reset to defaults: %s\n", configPath)
	return nil
}

func defaultConfigContent() string {
	var buf bytes.Buffer
	buf.WriteString("# term-llm configuration\n")
	buf.WriteString("# Run 'term-llm config edit' to modify\n\n")
	printAnnotatedConfigFiltered(&buf, config.GetDefaults(), map[string]bool{}, map[string]bool{}, nil, false, nil, true)
	return buf.String()
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
	if isApprovalConfigKey(key) {
		if _, err := parseConfiguredApprovalMode(key, value); err != nil {
			return err
		}
	}

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

	if err := config.WriteFileAtomically(configPath, buf.Bytes(), 0644); err != nil {
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
	printEffectiveApproval := func() (bool, error) {
		cfg, loadErr := config.Load()
		if loadErr != nil {
			return false, loadErr
		}
		value, ok, resolveErr := effectiveApprovalConfigValue(key, cfg)
		if resolveErr != nil {
			return false, resolveErr
		}
		if ok {
			fmt.Println(value)
		}
		return ok, nil
	}

	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			if printed, effectiveErr := printEffectiveApproval(); effectiveErr != nil {
				return effectiveErr
			} else if printed {
				return nil
			}
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
		if printed, effectiveErr := printEffectiveApproval(); effectiveErr != nil {
			return effectiveErr
		} else if printed {
			return nil
		}
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
	cfg, _ := config.Load()

	keySet := make(map[string]bool)
	for _, spec := range config.ConfigKeySpecs() {
		keySet[spec.Path] = true
	}

	providerNames := make(map[string]bool)
	for _, name := range config.DefaultProviderNames() {
		providerNames[name] = true
	}
	for _, name := range llm.GetProviderNames(cfg) {
		providerNames[name] = true
	}
	var names []string
	for name := range providerNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, spec := range config.ProviderKeySpecs() {
			keySet["providers."+name+"."+spec.Path] = true
		}
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

func isApprovalConfigKey(key string) bool {
	switch key {
	case "approval.default_mode", "chat.approval_mode", "ask.approval_mode", "edit.approval_mode", "exec.approval_mode", "loop.approval_mode", "serve.approval_mode", "serve.mcp.approval_mode":
		return true
	default:
		return false
	}
}

// configValueCompletions returns completions for config values based on key
func configValueCompletions(key, toComplete string) []string {
	cfg, _ := config.Load()

	switch key {
	case "approval.default_mode", "chat.approval_mode", "ask.approval_mode", "edit.approval_mode", "exec.approval_mode", "loop.approval_mode", "serve.approval_mode", "serve.mcp.approval_mode":
		return filterPrefix([]string{"prompt", "auto"}, toComplete)

	case "default_provider", "exec.provider", "ask.provider", "chat.provider", "edit.provider", "guardian.provider":
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
		providers := []string{"gemini", "openai", "chatgpt", "xai", "venice", "flux", "openrouter", "debug"}
		var completions []string
		for _, p := range providers {
			if strings.HasPrefix(p, toComplete) {
				completions = append(completions, p)
			}
		}
		return completions

	case "search.provider":
		providers := []string{"duckduckgo", "exa", "exa_mcp", "perplexity", "parallel", "tavily", "brave", "google"}
		var completions []string
		for _, p := range providers {
			if strings.HasPrefix(p, toComplete) {
				completions = append(completions, p)
			}
		}
		return completions

	case "search.fetch_provider":
		providers := []string{"jina", "exa_mcp", "none"}
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
	}

	if isBoolConfigKey(key) {
		return filterPrefix([]string{"true", "false"}, toComplete)
	}

	// Check for provider model keys
	if strings.HasPrefix(key, "providers.") && strings.HasSuffix(key, ".model") {
		parts := strings.Split(key, ".")
		if len(parts) == 3 {
			provider := parts[1]
			models := llm.ResolveProviderModelIDs(provider)
			var completions []string
			for _, m := range models {
				if strings.HasPrefix(m, toComplete) {
					completions = append(completions, m)
				}
			}
			return completions
		}
	}

	// Check for provider fast model keys
	if strings.HasPrefix(key, "providers.") && strings.HasSuffix(key, ".fast_model") {
		parts := strings.Split(key, ".")
		if len(parts) == 3 {
			provider := parts[1]
			models := llm.ResolveProviderModelIDs(provider)
			var completions []string
			for _, m := range models {
				if strings.HasPrefix(m, toComplete) {
					completions = append(completions, m)
				}
			}
			return completions
		}
	}

	// Check for provider fast provider keys
	if strings.HasPrefix(key, "providers.") && strings.HasSuffix(key, ".fast_provider") {
		names := llm.GetProviderNames(cfg)
		var completions []string
		for _, name := range names {
			if strings.HasPrefix(name, toComplete) {
				completions = append(completions, name)
			}
		}
		return completions
	}

	// Check for provider reasoning keys
	if strings.HasPrefix(key, "providers.") && strings.HasSuffix(key, ".reasoning") {
		return filterPrefix([]string{"auto", "enabled", "disabled"}, toComplete)
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
	if key == "image.chatgpt.model" {
		return filterPrefix(llm.ImageProviderModels["chatgpt"], toComplete)
	}
	if key == "image.flux.model" {
		return filterPrefix(llm.ImageProviderModels["flux"], toComplete)
	}
	if key == "image.venice.model" {
		return filterPrefix(llm.ImageProviderModels["venice"], toComplete)
	}

	return nil
}

func isBoolConfigKey(key string) bool {
	for _, spec := range config.ConfigKeySpecs() {
		if spec.Path != key {
			continue
		}
		if _, ok := spec.Default.(bool); ok {
			return true
		}
		_, ok := spec.Placeholder.(bool)
		return ok
	}
	if strings.HasPrefix(key, "providers.") {
		parts := strings.SplitN(key, ".", 3)
		if len(parts) == 3 {
			for _, spec := range config.ProviderKeySpecs() {
				if spec.Path == parts[2] {
					_, ok := spec.Placeholder.(bool)
					return ok
				}
			}
		}
	}
	return false
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
