package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/spf13/viper"
)

// ProviderType defines the supported provider implementations
type ProviderType string

const (
	ProviderTypeAnthropic    ProviderType = "anthropic"
	ProviderTypeOpenAI       ProviderType = "openai"
	ProviderTypeChatGPT      ProviderType = "chatgpt"
	ProviderTypeGemini       ProviderType = "gemini"
	ProviderTypeGeminiCLI    ProviderType = "gemini-cli"
	ProviderTypeOpenRouter   ProviderType = "openrouter"
	ProviderTypeZen          ProviderType = "zen"
	ProviderTypeClaudeBin    ProviderType = "claude-bin"
	ProviderTypeOpenAICompat ProviderType = "openai_compatible"
	ProviderTypeXAI          ProviderType = "xai"
)

// builtInProviderTypes maps known provider names to their types
var builtInProviderTypes = map[string]ProviderType{
	"anthropic":  ProviderTypeAnthropic,
	"openai":     ProviderTypeOpenAI,
	"chatgpt":    ProviderTypeChatGPT,
	"gemini":     ProviderTypeGemini,
	"gemini-cli": ProviderTypeGeminiCLI,
	"openrouter": ProviderTypeOpenRouter,
	"zen":        ProviderTypeZen,
	"claude-bin": ProviderTypeClaudeBin,
	"xai":        ProviderTypeXAI,
}

// InferProviderType returns the provider type for a given provider name
// Explicit type takes precedence, then built-in names, then defaults to openai_compatible
func InferProviderType(name string, explicit ProviderType) ProviderType {
	if explicit != "" {
		return explicit
	}
	if t, ok := builtInProviderTypes[name]; ok {
		return t
	}
	return ProviderTypeOpenAICompat
}

// ProviderConfig is a unified configuration for any provider
type ProviderConfig struct {
	// Type of provider - inferred from key name for built-ins, required for custom
	Type ProviderType `mapstructure:"type"`

	// Common fields
	APIKey      string   `mapstructure:"api_key"`
	Model       string   `mapstructure:"model"`
	Models      []string `mapstructure:"models"`      // Available models for autocomplete
	Credentials string   `mapstructure:"credentials"` // "api_key", "codex", "gemini-cli"

	// Search behavior - nil means auto (use native if available)
	UseNativeSearch *bool `mapstructure:"use_native_search"`

	// OpenAI-compatible specific
	BaseURL string `mapstructure:"base_url"` // Base URL - /chat/completions is appended
	URL     string `mapstructure:"url"`      // Full URL - used as-is without appending endpoint

	// OpenRouter specific
	AppURL   string `mapstructure:"app_url"`
	AppTitle string `mapstructure:"app_title"`

	// Runtime fields (populated after credential resolution)
	ResolvedAPIKey string                              `mapstructure:"-"`
	AccountID      string                              `mapstructure:"-"`
	OAuthCreds     *credentials.GeminiOAuthCredentials `mapstructure:"-"`
	ResolvedURL    string                              `mapstructure:"-"` // Resolved URL (after srv:// lookup)

	// Lazy resolution tracking - these are resolved on-demand before inference
	needsLazyResolution bool `mapstructure:"-"`
}

type Config struct {
	DefaultProvider string                    `mapstructure:"default_provider"`
	Providers       map[string]ProviderConfig `mapstructure:"providers"`
	Diagnostics     DiagnosticsConfig         `mapstructure:"diagnostics"`
	Exec            ExecConfig                `mapstructure:"exec"`
	Ask             AskConfig                 `mapstructure:"ask"`
	Chat            ChatConfig                `mapstructure:"chat"`
	Edit            EditConfig                `mapstructure:"edit"`
	Image           ImageConfig               `mapstructure:"image"`
	Search          SearchConfig              `mapstructure:"search"`
	Theme           ThemeConfig               `mapstructure:"theme"`
	Tools           ToolsConfig               `mapstructure:"tools"`
	Agents          AgentsConfig              `mapstructure:"agents"`
}

// AgentsConfig configures the agent system
type AgentsConfig struct {
	UseBuiltin  bool     `mapstructure:"use_builtin"`  // Enable built-in agents (default true)
	SearchPaths []string `mapstructure:"search_paths"` // Additional directories to search for agents
}

// ToolsConfig configures the local tool system
type ToolsConfig struct {
	Enabled         []string `mapstructure:"enabled"`            // Enabled tool names (CLI names)
	ReadDirs        []string `mapstructure:"read_dirs"`          // Directories for read operations
	WriteDirs       []string `mapstructure:"write_dirs"`         // Directories for write operations
	ShellAllow      []string `mapstructure:"shell_allow"`        // Shell command patterns
	ShellAutoRun    bool     `mapstructure:"shell_auto_run"`     // Auto-approve matching shell
	ShellAutoRunEnv string   `mapstructure:"shell_auto_run_env"` // Env var required for auto-run
	ShellNonTTYEnv  string   `mapstructure:"shell_non_tty_env"`  // Env var for non-TTY execution
	ImageProvider   string   `mapstructure:"image_provider"`     // Override for image provider
}

// DiagnosticsConfig configures diagnostic data collection
type DiagnosticsConfig struct {
	Enabled bool   `mapstructure:"enabled"` // Enable diagnostic data collection
	Dir     string `mapstructure:"dir"`     // Override default directory
}

// ThemeConfig allows customization of UI colors
// Colors can be ANSI color numbers (0-255) or hex codes (#RRGGBB)
type ThemeConfig struct {
	Primary   string `mapstructure:"primary"`   // main accent (commands, highlights)
	Secondary string `mapstructure:"secondary"` // secondary accent (headers, borders)
	Success   string `mapstructure:"success"`   // success states
	Error     string `mapstructure:"error"`     // error states
	Warning   string `mapstructure:"warning"`   // warnings
	Muted     string `mapstructure:"muted"`     // dimmed text
	Text      string `mapstructure:"text"`      // primary text
	Spinner   string `mapstructure:"spinner"`   // loading spinner
}

type ExecConfig struct {
	Provider     string `mapstructure:"provider"`     // Override provider for exec
	Model        string `mapstructure:"model"`        // Override model for exec
	Suggestions  int    `mapstructure:"suggestions"`  // Number of command suggestions (default 3)
	Instructions string `mapstructure:"instructions"` // Custom context for suggestions
}

type AskConfig struct {
	Provider     string `mapstructure:"provider"`     // Override provider for ask only
	Model        string `mapstructure:"model"`        // Override model for ask only
	Instructions string `mapstructure:"instructions"` // Custom system prompt for ask
	MaxTurns     int    `mapstructure:"max_turns"`    // Max agentic turns (default 20)
}

type ChatConfig struct {
	Provider     string `mapstructure:"provider"`     // Override provider for chat only
	Model        string `mapstructure:"model"`        // Override model for chat only
	Instructions string `mapstructure:"instructions"` // Custom system prompt for chat
	MaxTurns     int    `mapstructure:"max_turns"`    // Max agentic turns (default 200)
}

type EditConfig struct {
	Provider        string `mapstructure:"provider"`          // Override provider for edit
	Model           string `mapstructure:"model"`             // Override model for edit
	Instructions    string `mapstructure:"instructions"`      // Custom instructions for edits
	ShowLineNumbers bool   `mapstructure:"show_line_numbers"` // Show line numbers in diff
	ContextLines    int    `mapstructure:"context_lines"`     // Lines of context in diff
	Editor          string `mapstructure:"editor"`            // Override $EDITOR
	DiffFormat      string `mapstructure:"diff_format"`       // "auto", "udiff", or "replace" (default: auto)
}

// ImageConfig configures image generation settings
type ImageConfig struct {
	Provider   string                `mapstructure:"provider"`   // default image provider: gemini, openai, xai, flux, openrouter
	OutputDir  string                `mapstructure:"output_dir"` // default save directory
	Gemini     ImageGeminiConfig     `mapstructure:"gemini"`
	OpenAI     ImageOpenAIConfig     `mapstructure:"openai"`
	XAI        ImageXAIConfig        `mapstructure:"xai"`
	Flux       ImageFluxConfig       `mapstructure:"flux"`
	OpenRouter ImageOpenRouterConfig `mapstructure:"openrouter"`
}

// ImageGeminiConfig configures Gemini image generation
type ImageGeminiConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
}

// ImageOpenAIConfig configures OpenAI image generation
type ImageOpenAIConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
}

// ImageXAIConfig configures xAI (Grok) image generation
type ImageXAIConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // grok-2-image or grok-2-image-1212
}

// ImageFluxConfig configures Flux (Black Forest Labs) image generation
type ImageFluxConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // flux-2-pro for generation, flux-kontext-pro for editing
}

// ImageOpenRouterConfig configures OpenRouter image generation
type ImageOpenRouterConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // e.g., google/gemini-2.5-flash-image
}

// SearchConfig configures web search providers
type SearchConfig struct {
	Provider      string             `mapstructure:"provider"`       // exa, brave, google, duckduckgo (default)
	ForceExternal bool               `mapstructure:"force_external"` // force external search for all providers
	Exa           SearchExaConfig    `mapstructure:"exa"`
	Brave         SearchBraveConfig  `mapstructure:"brave"`
	Google        SearchGoogleConfig `mapstructure:"google"`
}

// SearchExaConfig configures Exa search
type SearchExaConfig struct {
	APIKey string `mapstructure:"api_key"`
}

// SearchBraveConfig configures Brave search
type SearchBraveConfig struct {
	APIKey string `mapstructure:"api_key"`
}

// SearchGoogleConfig configures Google Custom Search
type SearchGoogleConfig struct {
	APIKey string `mapstructure:"api_key"`
	CX     string `mapstructure:"cx"` // Custom Search Engine ID
}

func Load() (*Config, error) {
	configPath, err := GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get config dir: %w", err)
	}

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(configPath)
	viper.AddConfigPath(".")

	viper.RegisterAlias("provider", "default_provider")

	// Set defaults
	viper.SetDefault("default_provider", "anthropic")
	viper.SetDefault("exec.suggestions", 3)
	viper.SetDefault("ask.max_turns", 20)
	viper.SetDefault("chat.max_turns", 200)
	viper.SetDefault("edit.show_line_numbers", true)
	viper.SetDefault("edit.context_lines", 3)
	viper.SetDefault("edit.diff_format", "auto")
	// Provider defaults
	viper.SetDefault("providers.anthropic.model", "claude-sonnet-4-5")
	viper.SetDefault("providers.openai.model", "gpt-5.2")
	viper.SetDefault("providers.xai.model", "grok-4-1-fast")
	viper.SetDefault("providers.openrouter.model", "x-ai/grok-code-fast-1")
	viper.SetDefault("providers.openrouter.app_url", "https://github.com/samsaffron/term-llm")
	viper.SetDefault("providers.openrouter.app_title", "term-llm")
	viper.SetDefault("providers.gemini.model", "gemini-3-flash-preview")
	viper.SetDefault("providers.zen.model", "minimax-m2.1-free")
	// Image defaults
	viper.SetDefault("image.provider", "gemini")
	viper.SetDefault("image.output_dir", "~/Pictures/term-llm")
	viper.SetDefault("image.gemini.model", "gemini-2.5-flash-image")
	viper.SetDefault("image.openai.model", "gpt-image-1")
	viper.SetDefault("image.xai.model", "grok-2-image-1212")
	viper.SetDefault("image.flux.model", "flux-2-pro")
	viper.SetDefault("image.openrouter.model", "google/gemini-2.5-flash-image")
	// Search defaults
	viper.SetDefault("search.provider", "duckduckgo")
	viper.SetDefault("search.force_external", false)
	// Tools defaults
	viper.SetDefault("tools.enabled", []string{})
	viper.SetDefault("tools.read_dirs", []string{})
	viper.SetDefault("tools.write_dirs", []string{})
	viper.SetDefault("tools.shell_allow", []string{})
	viper.SetDefault("tools.shell_auto_run", false)
	viper.SetDefault("tools.shell_auto_run_env", "TERM_LLM_ALLOW_AUTORUN")
	viper.SetDefault("tools.shell_non_tty_env", "TERM_LLM_ALLOW_NON_TTY")
	// Agents defaults
	viper.SetDefault("agents.use_builtin", true)
	viper.SetDefault("agents.search_paths", []string{})

	// Read config file (optional - won't error if missing)
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config: %w", err)
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Initialize providers map if nil
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}

	// Resolve credentials for all providers
	for name, providerCfg := range cfg.Providers {
		if err := resolveProviderCredentials(name, &providerCfg); err != nil {
			return nil, fmt.Errorf("%s credentials: %w", name, err)
		}
		cfg.Providers[name] = providerCfg
	}

	resolveImageCredentials(&cfg.Image)
	resolveSearchCredentials(&cfg.Search)

	return &cfg, nil
}

// GetBuiltInProviderNames returns a list of all built-in provider type names.
func GetBuiltInProviderNames() []string {
	names := make([]string, 0, len(builtInProviderTypes))
	for name := range builtInProviderTypes {
		names = append(names, name)
	}
	return names
}

// ApplyOverrides applies provider and model overrides to the config.
// If provider is non-empty, it overrides the global provider.
// If model is non-empty, it overrides the model for the active provider.
func (c *Config) ApplyOverrides(provider, model string) {
	if provider != "" {
		c.DefaultProvider = provider
	}
	if model != "" && c.DefaultProvider != "" {
		cfg, ok := c.Providers[c.DefaultProvider]
		if !ok {
			// Initialize new provider config if it doesn't exist
			cfg = ProviderConfig{
				Model: model,
			}
		} else {
			cfg.Model = model
		}
		c.Providers[c.DefaultProvider] = cfg
	}
}

// GetProviderConfig returns the config for the specified provider name.
// Returns nil if the provider is not configured.
func (c *Config) GetProviderConfig(name string) *ProviderConfig {
	if cfg, ok := c.Providers[name]; ok {
		return &cfg
	}
	return nil
}

// GetActiveProviderConfig returns the config for the default provider.
// Returns nil if the default provider is not configured.
func (c *Config) GetActiveProviderConfig() *ProviderConfig {
	return c.GetProviderConfig(c.DefaultProvider)
}

// needsLazyResolve checks if a value requires expensive resolution (1Password, commands, SRV)
func needsLazyResolve(value string) bool {
	return strings.HasPrefix(value, "op://") ||
		strings.HasPrefix(value, "srv://") ||
		(strings.HasPrefix(value, "$(") && strings.HasSuffix(value, ")"))
}

// resolveProviderCredentials resolves credentials for a provider based on its type.
// Expensive operations (op://, srv://, $()) are deferred - call ResolveForInference() before use.
func resolveProviderCredentials(name string, cfg *ProviderConfig) error {
	providerType := InferProviderType(name, cfg.Type)

	// Check if URL fields need lazy resolution
	if needsLazyResolve(cfg.BaseURL) || needsLazyResolve(cfg.URL) {
		cfg.needsLazyResolution = true
	} else {
		// Resolve URL fields immediately (only env var expansion)
		cfg.BaseURL = expandEnv(cfg.BaseURL)
		cfg.URL = expandEnv(cfg.URL)
	}

	// Expand environment variables in other fields
	cfg.AppURL = expandEnv(cfg.AppURL)
	cfg.AppTitle = expandEnv(cfg.AppTitle)

	// Check if api_key uses magic syntax (op://, $(), etc.)
	// If so, defer resolution until inference time
	if cfg.APIKey != "" && needsLazyResolve(cfg.APIKey) {
		cfg.needsLazyResolution = true
		return nil
	}

	// Provider-specific credential resolution (non-lazy)
	switch providerType {
	case ProviderTypeAnthropic:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("ANTHROPIC_API_KEY")
		}

	case ProviderTypeOpenAI:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("OPENAI_API_KEY")
		}

	case ProviderTypeGemini:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("GEMINI_API_KEY")
		}

	case ProviderTypeGeminiCLI:
		creds, err := credentials.GetGeminiOAuthCredentials()
		if err != nil {
			return err
		}
		cfg.OAuthCreds = creds

	case ProviderTypeOpenRouter:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("OPENROUTER_API_KEY")
		}

	case ProviderTypeZen:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("ZEN_API_KEY")
		}
		// Empty API key is valid for free tier

	case ProviderTypeXAI:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("XAI_API_KEY")
		}

	case ProviderTypeOpenAICompat:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			// Try provider-specific env var (e.g., CEREBRAS_API_KEY for "cerebras")
			envName := strings.ToUpper(name) + "_API_KEY"
			cfg.ResolvedAPIKey = os.Getenv(envName)
		}
	}

	return nil
}

// ResolveForInference performs lazy resolution of expensive config values (op://, srv://, $()).
// Call this before creating a provider for inference.
func (cfg *ProviderConfig) ResolveForInference() error {
	if !cfg.needsLazyResolution {
		return nil
	}

	var err error

	// Resolve URL fields
	if needsLazyResolve(cfg.BaseURL) {
		cfg.ResolvedURL, err = ResolveValue(cfg.BaseURL)
		if err != nil {
			return fmt.Errorf("base_url: %w", err)
		}
	}
	if needsLazyResolve(cfg.URL) {
		cfg.ResolvedURL, err = ResolveValue(cfg.URL)
		if err != nil {
			return fmt.Errorf("url: %w", err)
		}
	}

	// Resolve API key
	if needsLazyResolve(cfg.APIKey) {
		cfg.ResolvedAPIKey, err = ResolveValue(cfg.APIKey)
		if err != nil {
			return fmt.Errorf("api_key: %w", err)
		}
	}

	cfg.needsLazyResolution = false
	return nil
}

// resolveImageCredentials resolves API credentials for all image providers
func resolveImageCredentials(cfg *ImageConfig) {
	// Gemini image credentials
	cfg.Gemini.APIKey = expandEnv(cfg.Gemini.APIKey)
	if cfg.Gemini.APIKey == "" {
		cfg.Gemini.APIKey = os.Getenv("GEMINI_API_KEY")
	}

	// OpenAI image credentials
	cfg.OpenAI.APIKey = expandEnv(cfg.OpenAI.APIKey)
	if cfg.OpenAI.APIKey == "" {
		cfg.OpenAI.APIKey = os.Getenv("OPENAI_API_KEY")
	}

	// xAI image credentials
	cfg.XAI.APIKey = expandEnv(cfg.XAI.APIKey)
	if cfg.XAI.APIKey == "" {
		cfg.XAI.APIKey = os.Getenv("XAI_API_KEY")
	}

	// Flux (BFL) image credentials
	cfg.Flux.APIKey = expandEnv(cfg.Flux.APIKey)
	if cfg.Flux.APIKey == "" {
		cfg.Flux.APIKey = os.Getenv("BFL_API_KEY")
	}

	// OpenRouter image credentials
	cfg.OpenRouter.APIKey = expandEnv(cfg.OpenRouter.APIKey)
	if cfg.OpenRouter.APIKey == "" {
		cfg.OpenRouter.APIKey = os.Getenv("OPENROUTER_API_KEY")
	}
}

// resolveSearchCredentials resolves API credentials for all search providers
func resolveSearchCredentials(cfg *SearchConfig) {
	// Exa credentials
	cfg.Exa.APIKey = expandEnv(cfg.Exa.APIKey)
	if cfg.Exa.APIKey == "" {
		cfg.Exa.APIKey = os.Getenv("EXA_API_KEY")
	}

	// Brave credentials
	cfg.Brave.APIKey = expandEnv(cfg.Brave.APIKey)
	if cfg.Brave.APIKey == "" {
		cfg.Brave.APIKey = os.Getenv("BRAVE_API_KEY")
	}

	// Google credentials
	cfg.Google.APIKey = expandEnv(cfg.Google.APIKey)
	if cfg.Google.APIKey == "" {
		cfg.Google.APIKey = os.Getenv("GOOGLE_SEARCH_API_KEY")
	}
	cfg.Google.CX = expandEnv(cfg.Google.CX)
	if cfg.Google.CX == "" {
		cfg.Google.CX = os.Getenv("GOOGLE_SEARCH_CX")
	}
}

// expandEnv expands ${VAR} or $VAR in a string
func expandEnv(s string) string {
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		varName := s[2 : len(s)-1]
		return os.Getenv(varName)
	}
	if strings.HasPrefix(s, "$") {
		return os.Getenv(s[1:])
	}
	return s
}

// GetConfigDir returns the XDG config directory for term-llm.
// Uses $XDG_CONFIG_HOME if set, otherwise ~/.config
func GetConfigDir() (string, error) {
	if xdgHome := os.Getenv("XDG_CONFIG_HOME"); xdgHome != "" {
		return filepath.Join(xdgHome, "term-llm"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".config", "term-llm"), nil
}

// GetConfigPath returns the path where the config file should be located
func GetConfigPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "config.yaml"), nil
}

// GetDiagnosticsDir returns the XDG data directory for term-llm diagnostics.
// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share
func GetDiagnosticsDir() string {
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "term-llm", "diagnostics")
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "term-llm-diagnostics") // fallback
	}
	return filepath.Join(homeDir, ".local", "share", "term-llm", "diagnostics")
}

// Exists returns true if a config file exists
func Exists() bool {
	path, err := GetConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// NeedsSetup returns true if config file doesn't exist
func NeedsSetup() bool {
	return !Exists()
}

// Save writes the config to disk
func Save(cfg *Config) error {
	path, err := GetConfigPath()
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Build providers section
	var providers strings.Builder
	providers.WriteString("providers:\n")
	for name, p := range cfg.Providers {
		providers.WriteString(fmt.Sprintf("  %s:\n", name))
		if p.Type != "" {
			providers.WriteString(fmt.Sprintf("    type: %s\n", p.Type))
		}
		if p.Model != "" {
			providers.WriteString(fmt.Sprintf("    model: %s\n", p.Model))
		}
		if p.BaseURL != "" {
			providers.WriteString(fmt.Sprintf("    base_url: %s\n", p.BaseURL))
		}
		if p.AppURL != "" {
			providers.WriteString(fmt.Sprintf("    app_url: %s\n", p.AppURL))
		}
		if p.AppTitle != "" {
			providers.WriteString(fmt.Sprintf("    app_title: %s\n", p.AppTitle))
		}
	}

	// Build image section if provider is set
	var imageSection string
	if cfg.Image.Provider != "" {
		imageSection = fmt.Sprintf(`
image:
  provider: %s
`, cfg.Image.Provider)
	}

	content := fmt.Sprintf(`default_provider: %s

exec:
  suggestions: %d
%s
%s`, cfg.DefaultProvider, cfg.Exec.Suggestions, imageSection, providers.String())

	return os.WriteFile(path, []byte(content), 0600)
}
