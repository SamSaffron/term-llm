package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Provider      string          `mapstructure:"provider"`
	SystemContext string          `mapstructure:"system_context"`
	Anthropic     AnthropicConfig `mapstructure:"anthropic"`
	OpenAI        OpenAIConfig    `mapstructure:"openai"`
}

type AnthropicConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
}

type OpenAIConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
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

	// Expand environment variables in API keys
	cfg.Anthropic.APIKey = expandEnv(cfg.Anthropic.APIKey)
	cfg.OpenAI.APIKey = expandEnv(cfg.OpenAI.APIKey)

	// Fall back to environment variables if API keys not set
	if cfg.Anthropic.APIKey == "" {
		cfg.Anthropic.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if cfg.OpenAI.APIKey == "" {
		cfg.OpenAI.APIKey = os.Getenv("OPENAI_API_KEY")
	}

	return &cfg, nil
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

# Custom context added to the system prompt (e.g., OS details, preferences)
system_context: |
  %s

anthropic:
  model: %s

openai:
  model: %s
`, cfg.Provider, cfg.SystemContext, cfg.Anthropic.Model, cfg.OpenAI.Model)

	return os.WriteFile(path, []byte(content), 0600)
}
