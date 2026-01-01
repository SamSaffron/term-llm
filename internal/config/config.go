package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/spf13/viper"
)

type Config struct {
	Provider  string          `mapstructure:"provider"`
	Exec      ExecConfig      `mapstructure:"exec"`
	Ask       AskConfig       `mapstructure:"ask"`
	Theme     ThemeConfig     `mapstructure:"theme"`
	Anthropic AnthropicConfig `mapstructure:"anthropic"`
	OpenAI    OpenAIConfig    `mapstructure:"openai"`
	Gemini    GeminiConfig    `mapstructure:"gemini"`
	Zen       ZenConfig       `mapstructure:"zen"`
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
	Suggestions  int    `mapstructure:"suggestions"`  // Number of command suggestions (default 3)
	Instructions string `mapstructure:"instructions"` // Custom context for suggestions
}

type AskConfig struct {
	Instructions string `mapstructure:"instructions"` // Custom system prompt for ask
}

type AnthropicConfig struct {
	APIKey      string `mapstructure:"api_key"`
	Model       string `mapstructure:"model"`
	Credentials string `mapstructure:"credentials"` // "api_key" (default) or "claude"
}

type OpenAIConfig struct {
	APIKey      string `mapstructure:"api_key"`
	Model       string `mapstructure:"model"`
	Credentials string `mapstructure:"credentials"` // "api_key" (default) or "codex"
	AccountID   string // Populated at runtime when using Codex OAuth credentials
}

type GeminiConfig struct {
	APIKey      string `mapstructure:"api_key"`
	Model       string `mapstructure:"model"`
	Credentials string `mapstructure:"credentials"` // "api_key" (default) or "gemini-cli"
	// OAuth credentials populated at runtime when using gemini-cli
	OAuthCreds *credentials.GeminiOAuthCredentials
}

// ZenConfig configures the OpenCode Zen provider
// Zen provides free access to models like GLM 4.7 via opencode.ai
// API key is optional - leave empty for free tier access
type ZenConfig struct {
	APIKey string `mapstructure:"api_key"` // Optional: leave empty for free tier
	Model  string `mapstructure:"model"`
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

	// Set defaults
	viper.SetDefault("provider", "anthropic")
	viper.SetDefault("exec.suggestions", 3)
	viper.SetDefault("anthropic.model", "claude-sonnet-4-5")
	viper.SetDefault("openai.model", "gpt-5.2")
	viper.SetDefault("gemini.model", "gemini-3-flash-preview")
	viper.SetDefault("zen.model", "glm-4.7-free")

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

	// Resolve API keys based on credentials setting
	if err := resolveAnthropicCredentials(&cfg.Anthropic); err != nil {
		return nil, fmt.Errorf("anthropic credentials: %w", err)
	}
	if err := resolveOpenAICredentials(&cfg.OpenAI); err != nil {
		return nil, fmt.Errorf("openai credentials: %w", err)
	}
	if err := resolveGeminiCredentials(&cfg.Gemini); err != nil {
		return nil, fmt.Errorf("gemini credentials: %w", err)
	}
	resolveZenCredentials(&cfg.Zen)

	return &cfg, nil
}

// resolveAnthropicCredentials resolves Anthropic API credentials
func resolveAnthropicCredentials(cfg *AnthropicConfig) error {
	switch cfg.Credentials {
	case "claude":
		token, err := credentials.GetClaudeToken()
		if err != nil {
			return err
		}
		cfg.APIKey = token
	default:
		// Default: "api_key" - use config value or environment variable
		cfg.APIKey = expandEnv(cfg.APIKey)
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		}
	}
	return nil
}

// resolveOpenAICredentials resolves OpenAI API credentials
func resolveOpenAICredentials(cfg *OpenAIConfig) error {
	switch cfg.Credentials {
	case "codex":
		creds, err := credentials.GetCodexCredentials()
		if err != nil {
			return err
		}
		cfg.APIKey = creds.AccessToken
		cfg.AccountID = creds.AccountID
	default:
		// Default: "api_key" - use config value or environment variable
		cfg.APIKey = expandEnv(cfg.APIKey)
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("OPENAI_API_KEY")
		}
	}
	return nil
}

// resolveGeminiCredentials resolves Gemini API credentials
func resolveGeminiCredentials(cfg *GeminiConfig) error {
	switch cfg.Credentials {
	case "gemini-cli":
		// Load OAuth credentials from gemini-cli
		creds, err := credentials.GetGeminiOAuthCredentials()
		if err != nil {
			return err
		}
		cfg.OAuthCreds = creds
	default:
		// Default: "api_key" - use config value or environment variable
		cfg.APIKey = expandEnv(cfg.APIKey)
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("GEMINI_API_KEY")
		}
	}
	return nil
}

// resolveZenCredentials resolves OpenCode Zen API credentials
// API key is optional - empty means free tier access
func resolveZenCredentials(cfg *ZenConfig) {
	cfg.APIKey = expandEnv(cfg.APIKey)
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("ZEN_API_KEY")
	}
	// Empty API key is valid - Zen offers free tier access
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

	content := fmt.Sprintf(`provider: %s

exec:
  suggestions: %d
  # Custom instructions for command suggestions (e.g., OS details, tool preferences)
  # instructions: |
  #   I use Arch Linux with zsh.
  #   Prefer ripgrep over grep, fd over find.

ask:
  # Custom system prompt for ask command
  # instructions: |
  #   Be concise. I'm an experienced developer.

anthropic:
  model: %s

openai:
  model: %s

gemini:
  model: %s

zen:
  model: %s
  # api_key: optional - leave empty for free tier access
  # Set ZEN_API_KEY env var or add api_key here if you have one
`, cfg.Provider, cfg.Exec.Suggestions, cfg.Anthropic.Model, cfg.OpenAI.Model, cfg.Gemini.Model, cfg.Zen.Model)

	return os.WriteFile(path, []byte(content), 0600)
}
