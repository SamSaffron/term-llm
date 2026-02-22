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
	ProviderTypeCopilot      ProviderType = "copilot"
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
	"copilot":    ProviderTypeCopilot,
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
	DebugLogs       DebugLogsConfig           `mapstructure:"debug_logs"`
	Sessions        SessionsConfig            `mapstructure:"sessions"`
	Exec            ExecConfig                `mapstructure:"exec"`
	Ask             AskConfig                 `mapstructure:"ask"`
	Chat            ChatConfig                `mapstructure:"chat"`
	Edit            EditConfig                `mapstructure:"edit"`
	Image           ImageConfig               `mapstructure:"image"`
	Embed           EmbedConfig               `mapstructure:"embed"`
	Search          SearchConfig              `mapstructure:"search"`
	Theme           ThemeConfig               `mapstructure:"theme"`
	Tools           ToolsConfig               `mapstructure:"tools"`
	Agents          AgentsConfig              `mapstructure:"agents"`
	Skills          SkillsConfig              `mapstructure:"skills"`
	AgentsMd        AgentsMdConfig            `mapstructure:"agents_md"`
	AutoCompact     bool                      `mapstructure:"auto_compact"`
	Serve           ServeConfig               `mapstructure:"serve"`
}

// ServeConfig holds configuration for the serve command platforms.
type ServeConfig struct {
	Telegram TelegramServeConfig `mapstructure:"telegram" yaml:"telegram,omitempty"`
}

// TelegramServeConfig holds configuration for the Telegram bot platform.
type TelegramServeConfig struct {
	Token            string   `mapstructure:"token" yaml:"token,omitempty"`
	AllowedUserIDs   []int64  `mapstructure:"allowed_user_ids" yaml:"allowed_user_ids,omitempty"`
	AllowedUsernames []string `mapstructure:"allowed_usernames" yaml:"allowed_usernames,omitempty"`
	IdleTimeout      int      `mapstructure:"idle_timeout" yaml:"idle_timeout,omitempty"`           // minutes
	InterruptTimeout int      `mapstructure:"interrupt_timeout" yaml:"interrupt_timeout,omitempty"` // seconds, 0 = default (3)
}

// AgentsConfig configures the agent system
type AgentsConfig struct {
	UseBuiltin  bool                       `mapstructure:"use_builtin"`  // Enable built-in agents (default true)
	SearchPaths []string                   `mapstructure:"search_paths"` // Additional directories to search for agents
	Preferences map[string]AgentPreference `mapstructure:"preferences"`  // Per-agent preference overrides
}

// AgentPreference allows overriding agent settings via config.yaml.
// All fields are optional - only set fields override the agent's defaults.
type AgentPreference struct {
	// Model preferences
	Provider string `mapstructure:"provider,omitempty" yaml:"provider,omitempty"`
	Model    string `mapstructure:"model,omitempty" yaml:"model,omitempty"`

	// Tool configuration
	ToolsEnabled  []string `mapstructure:"tools_enabled,omitempty" yaml:"tools_enabled,omitempty"`
	ToolsDisabled []string `mapstructure:"tools_disabled,omitempty" yaml:"tools_disabled,omitempty"`

	// Shell settings
	ShellAllow   []string `mapstructure:"shell_allow,omitempty" yaml:"shell_allow,omitempty"`
	ShellAutoRun *bool    `mapstructure:"shell_auto_run,omitempty" yaml:"shell_auto_run,omitempty"`

	// Spawn settings
	SpawnMaxParallel   *int     `mapstructure:"spawn_max_parallel,omitempty" yaml:"spawn_max_parallel,omitempty"`
	SpawnMaxDepth      *int     `mapstructure:"spawn_max_depth,omitempty" yaml:"spawn_max_depth,omitempty"`
	SpawnTimeout       *int     `mapstructure:"spawn_timeout,omitempty" yaml:"spawn_timeout,omitempty"`
	SpawnAllowedAgents []string `mapstructure:"spawn_allowed_agents,omitempty" yaml:"spawn_allowed_agents,omitempty"`

	// Behavior
	MaxTurns *int  `mapstructure:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	Search   *bool `mapstructure:"search,omitempty" yaml:"search,omitempty"`
}

// SkillsConfig configures the Agent Skills system
type SkillsConfig struct {
	Enabled              bool `mapstructure:"enabled"`                // Enable the skills system
	AutoInvoke           bool `mapstructure:"auto_invoke"`            // Allow model-driven activation
	MetadataBudgetTokens int  `mapstructure:"metadata_budget_tokens"` // Max tokens for skill metadata
	MaxActive            int  `mapstructure:"max_active"`             // Max skills in metadata injection

	IncludeProjectSkills  bool `mapstructure:"include_project_skills"`  // Discover from project-local paths
	IncludeEcosystemPaths bool `mapstructure:"include_ecosystem_paths"` // Include ~/.codex/skills, ~/.claude/skills, ~/.gemini/skills, .skills/

	AlwaysEnabled []string `mapstructure:"always_enabled"` // Always include in metadata
	NeverAuto     []string `mapstructure:"never_auto"`     // Must be explicit activation
}

// AgentsMdConfig configures optional AGENTS.md loading
type AgentsMdConfig struct {
	Enabled bool `mapstructure:"enabled"` // Load AGENTS.md into system prompt
}

// ToolsConfig configures the local tool system
type ToolsConfig struct {
	Enabled            []string `mapstructure:"enabled"`               // Enabled tool names (CLI names)
	ReadDirs           []string `mapstructure:"read_dirs"`             // Directories for read operations
	WriteDirs          []string `mapstructure:"write_dirs"`            // Directories for write operations
	ShellAllow         []string `mapstructure:"shell_allow"`           // Shell command patterns
	ShellAutoRun       bool     `mapstructure:"shell_auto_run"`        // Auto-approve matching shell
	ShellAutoRunEnv    string   `mapstructure:"shell_auto_run_env"`    // Env var required for auto-run
	ShellNonTTYEnv     string   `mapstructure:"shell_non_tty_env"`     // Env var for non-TTY execution
	ImageProvider      string   `mapstructure:"image_provider"`        // Override for image provider
	MaxToolOutputChars int      `mapstructure:"max_tool_output_chars"` // Global max chars per tool output (default 20000)
}

// DiagnosticsConfig configures diagnostic data collection
type DiagnosticsConfig struct {
	Enabled bool   `mapstructure:"enabled"` // Enable diagnostic data collection
	Dir     string `mapstructure:"dir"`     // Override default directory
}

// DebugLogsConfig configures debug logging of LLM requests and responses
type DebugLogsConfig struct {
	Enabled bool   `mapstructure:"enabled"` // Enable debug logging
	Dir     string `mapstructure:"dir"`     // Override default directory (defaults to ~/.local/share/term-llm/debug/)
}

// SessionsConfig configures session storage
type SessionsConfig struct {
	Enabled    bool   `mapstructure:"enabled"`      // Master switch - set to false to disable all session storage
	MaxAgeDays int    `mapstructure:"max_age_days"` // Auto-delete sessions older than N days (0=never)
	MaxCount   int    `mapstructure:"max_count"`    // Keep at most N sessions, delete oldest (0=unlimited)
	Path       string `mapstructure:"path"`         // Optional SQLite DB path override (supports :memory:)
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
	Provider   string                `mapstructure:"provider"`   // default image provider: gemini, openai, xai, flux, openrouter, debug
	OutputDir  string                `mapstructure:"output_dir"` // default save directory
	Gemini     ImageGeminiConfig     `mapstructure:"gemini"`
	OpenAI     ImageOpenAIConfig     `mapstructure:"openai"`
	XAI        ImageXAIConfig        `mapstructure:"xai"`
	Flux       ImageFluxConfig       `mapstructure:"flux"`
	OpenRouter ImageOpenRouterConfig `mapstructure:"openrouter"`
	Debug      ImageDebugConfig      `mapstructure:"debug"`
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

// ImageDebugConfig configures the debug image provider (local random images)
type ImageDebugConfig struct {
	Delay float64 `mapstructure:"delay"` // delay in seconds before returning (e.g., 1.5)
}

// EmbedConfig configures text embedding generation
type EmbedConfig struct {
	Provider string            `mapstructure:"provider"` // default embedding provider: gemini, openai, jina, voyage, ollama
	OpenAI   EmbedOpenAIConfig `mapstructure:"openai"`
	Gemini   EmbedGeminiConfig `mapstructure:"gemini"`
	Jina     EmbedJinaConfig   `mapstructure:"jina"`
	Voyage   EmbedVoyageConfig `mapstructure:"voyage"`
	Ollama   EmbedOllamaConfig `mapstructure:"ollama"`
}

// EmbedOpenAIConfig configures OpenAI embedding generation
type EmbedOpenAIConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // text-embedding-3-small (default), text-embedding-3-large
}

// EmbedGeminiConfig configures Gemini embedding generation
type EmbedGeminiConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // gemini-embedding-001 (default)
}

// EmbedJinaConfig configures Jina AI embedding generation
type EmbedJinaConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // jina-embeddings-v3 (default), jina-embeddings-v4
}

// EmbedVoyageConfig configures Voyage AI embedding generation
type EmbedVoyageConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // voyage-3.5 (default), voyage-3-large, voyage-code-3
}

// EmbedOllamaConfig configures Ollama embedding generation
type EmbedOllamaConfig struct {
	BaseURL string `mapstructure:"base_url"` // default: http://localhost:11434
	Model   string `mapstructure:"model"`    // nomic-embed-text (default)
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

	// Set defaults from GetDefaults() - single source of truth
	for key, value := range GetDefaults() {
		viper.SetDefault(key, value)
	}

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
	resolveEmbedCredentials(&cfg.Embed)
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

// DescribeCredentialSource returns a human-readable description of which credential
// source will be used for the given provider. This is used by `config show` to help
// users understand where their credentials are coming from.
// Returns a short label (e.g., "ANTHROPIC_API_KEY env") and whether any credential was found.
func DescribeCredentialSource(name string, cfg *ProviderConfig) (string, bool) {
	providerType := InferProviderType(name, cfg.Type)

	// If there's a lazy-resolved api_key (op://, $()), describe it
	if cfg.APIKey != "" && needsLazyResolve(cfg.APIKey) {
		return fmt.Sprintf("api_key (deferred: %s)", truncateValue(cfg.APIKey, 30)), true
	}

	switch providerType {
	case ProviderTypeAnthropic:
		return describeAnthropicCredential(cfg)
	case ProviderTypeOpenAI:
		return describeEnvKeyCredential(cfg, "OPENAI_API_KEY")
	case ProviderTypeGemini:
		return describeEnvKeyCredential(cfg, "GEMINI_API_KEY")
	case ProviderTypeGeminiCLI:
		if _, err := credentials.GetGeminiOAuthCredentials(); err == nil {
			return "gemini-cli OAuth (~/.gemini/oauth_creds.json)", true
		}
		return "gemini-cli OAuth (not found)", false
	case ProviderTypeOpenRouter:
		return describeEnvKeyCredential(cfg, "OPENROUTER_API_KEY")
	case ProviderTypeZen:
		source, found := describeEnvKeyCredential(cfg, "ZEN_API_KEY")
		if !found {
			return "none (free tier)", true // Zen works without a key
		}
		return source, found
	case ProviderTypeXAI:
		return describeEnvKeyCredential(cfg, "XAI_API_KEY")
	case ProviderTypeClaudeBin:
		return "claude-bin CLI (no key needed)", true
	case ProviderTypeChatGPT:
		return "ChatGPT OAuth (interactive)", true
	case ProviderTypeCopilot:
		return "GitHub Copilot OAuth (interactive)", true
	case ProviderTypeOpenAICompat:
		envName := strings.ToUpper(name) + "_API_KEY"
		return describeEnvKeyCredential(cfg, envName)
	}

	return "unknown", false
}

// describeAnthropicCredential walks the Anthropic credential cascade and returns
// a description of which source will be used. Mirrors the logic in NewAnthropicProvider.
func describeAnthropicCredential(cfg *ProviderConfig) (string, bool) {
	// 1. Explicit API key from config
	apiKey := expandEnv(cfg.APIKey)
	if apiKey != "" {
		return "config api_key", true
	}

	// 2. ANTHROPIC_API_KEY env
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "ANTHROPIC_API_KEY env", true
	}

	// 3. CLAUDE_CODE_OAUTH_TOKEN env
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return "CLAUDE_CODE_OAUTH_TOKEN env (OAuth)", true
	}

	// 4. Saved OAuth token
	if credentials.AnthropicOAuthCredentialsExist() {
		return "saved OAuth token (~/.config/term-llm/anthropic_oauth.json)", true
	}

	// 5. Would prompt interactively
	return "none (will prompt for OAuth token interactively)", false
}

// describeEnvKeyCredential checks config api_key then an environment variable.
func describeEnvKeyCredential(cfg *ProviderConfig, envName string) (string, bool) {
	apiKey := expandEnv(cfg.APIKey)
	if apiKey != "" {
		return "config api_key", true
	}
	if os.Getenv(envName) != "" {
		return envName + " env", true
	}
	return fmt.Sprintf("none (set %s or config api_key)", envName), false
}

// truncateValue truncates a string for display, adding "..." if too long.
func truncateValue(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
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

// resolveEmbedCredentials resolves API credentials for all embedding providers
func resolveEmbedCredentials(cfg *EmbedConfig) {
	// OpenAI embed credentials
	cfg.OpenAI.APIKey = expandEnv(cfg.OpenAI.APIKey)
	if cfg.OpenAI.APIKey == "" {
		cfg.OpenAI.APIKey = os.Getenv("OPENAI_API_KEY")
	}

	// Gemini embed credentials
	cfg.Gemini.APIKey = expandEnv(cfg.Gemini.APIKey)
	if cfg.Gemini.APIKey == "" {
		cfg.Gemini.APIKey = os.Getenv("GEMINI_API_KEY")
	}

	// Jina embed credentials
	cfg.Jina.APIKey = expandEnv(cfg.Jina.APIKey)
	if cfg.Jina.APIKey == "" {
		cfg.Jina.APIKey = os.Getenv("JINA_API_KEY")
	}

	// Voyage embed credentials
	cfg.Voyage.APIKey = expandEnv(cfg.Voyage.APIKey)
	if cfg.Voyage.APIKey == "" {
		cfg.Voyage.APIKey = os.Getenv("VOYAGE_API_KEY")
	}

	// Ollama base URL
	cfg.Ollama.BaseURL = expandEnv(cfg.Ollama.BaseURL)
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

// ParseProviderModel splits "provider:model" into separate parts.
// Returns (provider, model). Model will be empty if not specified.
// This is a simple version that doesn't validate against configured providers.
func ParseProviderModel(s string) (provider, model string) {
	parts := strings.SplitN(s, ":", 2)
	provider = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		model = strings.TrimSpace(parts[1])
	}
	return provider, model
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

// GetDebugLogsDir returns the XDG data directory for term-llm debug logs.
// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share
func GetDebugLogsDir() string {
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "term-llm", "debug")
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "term-llm-debug") // fallback
	}
	return filepath.Join(homeDir, ".local", "share", "term-llm", "debug")
}

// KnownKeys contains all valid configuration key paths
// Dynamic keys like providers.* and image.* have their subkeys validated separately
var KnownKeys = map[string]bool{
	// Top-level
	"default_provider": true,
	"providers":        true,
	"diagnostics":      true,
	"debug_logs":       true,
	"exec":             true,
	"ask":              true,
	"chat":             true,
	"edit":             true,
	"image":            true,
	"search":           true,
	"theme":            true,
	"tools":            true,
	"agents":           true,
	"skills":           true,
	"agents_md":        true,

	// Diagnostics
	"diagnostics.enabled": true,
	"diagnostics.dir":     true,

	// Debug logs
	"debug_logs.enabled": true,
	"debug_logs.dir":     true,

	// Sessions
	"sessions":              true,
	"sessions.enabled":      true,
	"sessions.max_age_days": true,
	"sessions.max_count":    true,
	"sessions.path":         true,

	// Exec
	"exec.provider":     true,
	"exec.model":        true,
	"exec.suggestions":  true,
	"exec.instructions": true,

	// Ask
	"ask.provider":     true,
	"ask.model":        true,
	"ask.instructions": true,
	"ask.max_turns":    true,

	// Chat
	"chat.provider":     true,
	"chat.model":        true,
	"chat.instructions": true,
	"chat.max_turns":    true,

	// Edit
	"edit.provider":          true,
	"edit.model":             true,
	"edit.instructions":      true,
	"edit.show_line_numbers": true,
	"edit.context_lines":     true,
	"edit.editor":            true,
	"edit.diff_format":       true,

	// Image
	"image.provider":           true,
	"image.output_dir":         true,
	"image.gemini":             true,
	"image.gemini.api_key":     true,
	"image.gemini.model":       true,
	"image.openai":             true,
	"image.openai.api_key":     true,
	"image.openai.model":       true,
	"image.xai":                true,
	"image.xai.api_key":        true,
	"image.xai.model":          true,
	"image.flux":               true,
	"image.flux.api_key":       true,
	"image.flux.model":         true,
	"image.openrouter":         true,
	"image.openrouter.api_key": true,
	"image.openrouter.model":   true,
	"image.debug":              true,
	"image.debug.delay":        true,

	// Embed
	"embed.provider":        true,
	"embed.openai":          true,
	"embed.openai.api_key":  true,
	"embed.openai.model":    true,
	"embed.gemini":          true,
	"embed.gemini.api_key":  true,
	"embed.gemini.model":    true,
	"embed.jina":            true,
	"embed.jina.api_key":    true,
	"embed.jina.model":      true,
	"embed.voyage":          true,
	"embed.voyage.api_key":  true,
	"embed.voyage.model":    true,
	"embed.ollama":          true,
	"embed.ollama.base_url": true,
	"embed.ollama.model":    true,

	// Search
	"search.provider":       true,
	"search.force_external": true,
	"search.exa":            true,
	"search.exa.api_key":    true,
	"search.brave":          true,
	"search.brave.api_key":  true,
	"search.google":         true,
	"search.google.api_key": true,
	"search.google.cx":      true,

	// Theme
	"theme.primary":   true,
	"theme.secondary": true,
	"theme.success":   true,
	"theme.error":     true,
	"theme.warning":   true,
	"theme.muted":     true,
	"theme.text":      true,
	"theme.spinner":   true,

	// Tools
	"tools.enabled":               true,
	"tools.read_dirs":             true,
	"tools.write_dirs":            true,
	"tools.shell_allow":           true,
	"tools.shell_auto_run":        true,
	"tools.shell_auto_run_env":    true,
	"tools.shell_non_tty_env":     true,
	"tools.image_provider":        true,
	"tools.max_tool_output_chars": true,

	// Agents
	"agents.use_builtin":  true,
	"agents.search_paths": true,
	"agents.preferences":  true,

	// Skills
	"skills.enabled":                 true,
	"skills.auto_invoke":             true,
	"skills.metadata_budget_tokens":  true,
	"skills.max_active":              true,
	"skills.include_project_skills":  true,
	"skills.include_ecosystem_paths": true,
	"skills.always_enabled":          true,
	"skills.never_auto":              true,

	// AGENTS.md
	"agents_md.enabled": true,

	// Auto-compaction
	"auto_compact": true,
}

// KnownProviderKeys contains valid keys for provider configurations
var KnownProviderKeys = map[string]bool{
	"type":              true,
	"api_key":           true,
	"model":             true,
	"models":            true,
	"credentials":       true,
	"use_native_search": true,
	"base_url":          true,
	"url":               true,
	"app_url":           true,
	"app_title":         true,
}

// GetDefaults returns a map of all default configuration values
func GetDefaults() map[string]any {
	return map[string]any{
		"default_provider":               "anthropic",
		"exec.suggestions":               3,
		"exec.instructions":              "",
		"ask.max_turns":                  20,
		"ask.instructions":               "You are a helpful assistant. Today's date is {{date}}.",
		"chat.max_turns":                 200,
		"chat.instructions":              "You are a helpful assistant. Today's date is {{date}}.",
		"edit.show_line_numbers":         true,
		"edit.instructions":              "",
		"edit.context_lines":             3,
		"edit.diff_format":               "auto",
		"providers.anthropic.model":      "claude-sonnet-4-6",
		"providers.openai.model":         "gpt-5.2",
		"providers.xai.model":            "grok-4-1-fast",
		"providers.openrouter.model":     "x-ai/grok-code-fast-1",
		"providers.openrouter.app_url":   "https://github.com/samsaffron/term-llm",
		"providers.openrouter.app_title": "term-llm",
		"providers.gemini.model":         "gemini-3-flash-preview",
		"providers.zen.model":            "minimax-m2.1-free",
		"image.provider":                 "gemini",
		"image.output_dir":               "~/Pictures/term-llm",
		"image.gemini.model":             "gemini-2.5-flash-image",
		"image.openai.model":             "gpt-image-1",
		"image.xai.model":                "grok-2-image-1212",
		"image.flux.model":               "flux-2-pro",
		"image.openrouter.model":         "google/gemini-2.5-flash-image",
		"image.debug.delay":              0.0,
		"embed.openai.model":             "text-embedding-3-small",
		"embed.gemini.model":             "gemini-embedding-001",
		"embed.jina.model":               "jina-embeddings-v3",
		"embed.voyage.model":             "voyage-3.5",
		"embed.ollama.model":             "nomic-embed-text",
		"embed.ollama.base_url":          "http://localhost:11434",
		"search.provider":                "duckduckgo",
		"search.force_external":          false,
		"tools.enabled":                  []string{},
		"tools.read_dirs":                []string{},
		"tools.write_dirs":               []string{},
		"tools.shell_allow":              []string{},
		"tools.shell_auto_run":           false,
		"tools.shell_auto_run_env":       "TERM_LLM_ALLOW_AUTORUN",
		"tools.shell_non_tty_env":        "TERM_LLM_ALLOW_NON_TTY",
		"tools.max_tool_output_chars":    20000,
		"sessions.enabled":               true,
		"sessions.max_age_days":          0,
		"sessions.max_count":             0,
		"sessions.path":                  "",
		"agents.use_builtin":             true,
		"agents.search_paths":            []string{},
		"skills.enabled":                 false,
		"skills.auto_invoke":             true,
		"skills.metadata_budget_tokens":  8000,
		"skills.max_active":              50,
		"skills.include_project_skills":  true,
		"skills.include_ecosystem_paths": true,
		"skills.always_enabled":          []string{},
		"skills.never_auto":              []string{},
		"agents_md.enabled":              false,
		"auto_compact":                   false,
	}
}

// KnownAgentPreferenceKeys contains valid keys for agent preference configurations
var KnownAgentPreferenceKeys = map[string]bool{
	"provider":             true,
	"model":                true,
	"tools_enabled":        true,
	"tools_disabled":       true,
	"shell_allow":          true,
	"shell_auto_run":       true,
	"spawn_max_parallel":   true,
	"spawn_max_depth":      true,
	"spawn_timeout":        true,
	"spawn_allowed_agents": true,
	"max_turns":            true,
	"search":               true,
}

// IsKnownKey checks if a key path is a known configuration key
// For provider keys (providers.*), validates the sub-keys
// For agent preference keys (agents.preferences.*), validates the sub-keys
func IsKnownKey(keyPath string) bool {
	// Check direct match
	if KnownKeys[keyPath] {
		return true
	}

	// Check for providers.* pattern
	if strings.HasPrefix(keyPath, "providers.") {
		parts := strings.SplitN(keyPath, ".", 3)
		if len(parts) == 2 {
			// providers.<name> is always valid
			return true
		}
		if len(parts) == 3 {
			// providers.<name>.<key> - check if <key> is valid
			return KnownProviderKeys[parts[2]]
		}
	}

	// Check for agents.preferences.* pattern
	if strings.HasPrefix(keyPath, "agents.preferences.") {
		parts := strings.SplitN(keyPath, ".", 4)
		if len(parts) == 3 {
			// agents.preferences.<agent-name> is always valid
			return true
		}
		if len(parts) == 4 {
			// agents.preferences.<agent-name>.<key> - check if <key> is valid
			return KnownAgentPreferenceKeys[parts[3]]
		}
	}

	return false
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

// SetAgentPreference sets a preference for a specific agent.
// Uses viper to merge with existing config.
// Supports "provider:model" format for the provider key (e.g., "chatgpt:gpt-5.2-codex").
// Returns a list of keys that were set (may be multiple for provider:model format).
func SetAgentPreference(agentName, key, value string) ([]string, error) {
	// Validate the key
	if !KnownAgentPreferenceKeys[key] {
		return nil, fmt.Errorf("unknown agent preference key: %s", key)
	}

	configPath, err := GetConfigPath()
	if err != nil {
		return nil, err
	}

	// Ensure config directory exists
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	// Load existing config using a separate viper instance
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	// Try to read existing config (ignore if doesn't exist)
	_ = v.ReadInConfig()

	var keysSet []string

	// Handle provider:model format
	if key == "provider" && strings.Contains(value, ":") {
		provider, model := ParseProviderModel(value)

		providerKey := fmt.Sprintf("agents.preferences.%s.provider", agentName)
		modelKey := fmt.Sprintf("agents.preferences.%s.model", agentName)

		v.Set(providerKey, provider)
		v.Set(modelKey, model)
		keysSet = append(keysSet, "provider", "model")
	} else {
		// Set the preference
		viperKey := fmt.Sprintf("agents.preferences.%s.%s", agentName, key)

		// Parse value based on key type
		switch key {
		case "max_turns", "spawn_max_parallel", "spawn_max_depth", "spawn_timeout":
			// Integer values
			var intVal int
			if _, err := fmt.Sscanf(value, "%d", &intVal); err != nil {
				return nil, fmt.Errorf("invalid integer value for %s: %s", key, value)
			}
			if intVal < 0 {
				return nil, fmt.Errorf("negative value not allowed for %s: %d", key, intVal)
			}
			v.Set(viperKey, intVal)
		case "search", "shell_auto_run":
			// Boolean values (case-insensitive)
			lowerVal := strings.ToLower(value)
			boolVal := lowerVal == "true" || value == "1" || lowerVal == "yes"
			v.Set(viperKey, boolVal)
		case "tools_enabled", "tools_disabled", "shell_allow", "spawn_allowed_agents":
			// Array values (comma-separated)
			if value == "" {
				v.Set(viperKey, []string{})
			} else {
				parts := strings.Split(value, ",")
				for i := range parts {
					parts[i] = strings.TrimSpace(parts[i])
				}
				v.Set(viperKey, parts)
			}
		default:
			// String values
			v.Set(viperKey, value)
		}
		keysSet = append(keysSet, key)
	}

	return keysSet, v.WriteConfig()
}

// GetAgentPreference returns the preferences for a specific agent.
func GetAgentPreference(agentName string) (AgentPreference, bool) {
	cfg, err := Load()
	if err != nil {
		return AgentPreference{}, false
	}

	if cfg.Agents.Preferences == nil {
		return AgentPreference{}, false
	}

	pref, ok := cfg.Agents.Preferences[agentName]
	return pref, ok
}

// ClearAgentPreferences removes all preferences for a specific agent.
func ClearAgentPreferences(agentName string) error {
	configPath, err := GetConfigPath()
	if err != nil {
		return err
	}

	// Load existing config
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			return nil // Nothing to clear
		}
		return err
	}

	// Get all preferences
	prefs := v.GetStringMap("agents.preferences")
	if prefs == nil {
		return nil // Nothing to clear
	}

	// Remove this agent's preferences
	delete(prefs, agentName)

	// Set the updated preferences map
	if len(prefs) == 0 {
		// Remove the entire preferences section if empty
		v.Set("agents.preferences", nil)
	} else {
		v.Set("agents.preferences", prefs)
	}

	return v.WriteConfig()
}

// SetServeTelegramConfig saves Telegram bot configuration using viper.
// Merges with existing config rather than overwriting.
func SetServeTelegramConfig(c TelegramServeConfig) error {
	configPath, err := GetConfigPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")
	_ = v.ReadInConfig()

	v.Set("serve.telegram.token", c.Token)
	v.Set("serve.telegram.allowed_user_ids", c.AllowedUserIDs)
	v.Set("serve.telegram.allowed_usernames", c.AllowedUsernames)
	if c.IdleTimeout > 0 {
		v.Set("serve.telegram.idle_timeout", c.IdleTimeout)
	}
	if c.InterruptTimeout > 0 {
		v.Set("serve.telegram.interrupt_timeout", c.InterruptTimeout)
	}

	return v.WriteConfig()
}
