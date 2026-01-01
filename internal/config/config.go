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
	Anthropic AnthropicConfig `mapstructure:"anthropic"`
	OpenAI    OpenAIConfig    `mapstructure:"openai"`
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

func Load() (*Config, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get config dir: %w", err)
	}

	configPath := filepath.Join(configDir, "term-llm")

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(configPath)
	viper.AddConfigPath(".")

	// Set defaults
	viper.SetDefault("provider", "anthropic")
	viper.SetDefault("exec.suggestions", 3)
	viper.SetDefault("anthropic.model", "claude-sonnet-4-5")
	viper.SetDefault("openai.model", "gpt-5.2")

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

// GetConfigPath returns the path where the config file should be located
func GetConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "term-llm", "config.yaml"), nil
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
`, cfg.Provider, cfg.Exec.Suggestions, cfg.Anthropic.Model, cfg.OpenAI.Model)

	return os.WriteFile(path, []byte(content), 0600)
}
