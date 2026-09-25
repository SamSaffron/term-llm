package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/samsaffron/term-llm/internal/filelock"
)

var configUpdateMu sync.Mutex

// Config represents the mcp.json configuration file.
type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

// ServerConfig represents a configured MCP server.
// Supports both stdio transport (Command/Args) and HTTP transport (URL).
type ServerConfig struct {
	// Type discriminator: "stdio" (default if command present) or "http"
	Type string `json:"type,omitempty"`

	// Stdio transport fields
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`

	// HTTP transport fields
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// OAuth configures automatic authorization-code authentication for HTTP
	// transports. An explicit Authorization header remains authoritative.
	OAuth *OAuthConfig `json:"oauth,omitempty"`

	// Shared fields
	Env        map[string]string `json:"env,omitempty"`
	AlwaysLoad []string          `json:"always_load,omitempty"`

	// Sampling configuration
	Sampling *SamplingConfig `json:"sampling,omitempty"`
}

// OAuthConfig customizes automatic OAuth for a remote MCP server. Client
// secrets should be referenced through the environment or a deferred value
// (op://, file://, $()) rather than stored literally in mcp.json.
type OAuthConfig struct {
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret supports deferred resolution (op://, file://, $(), ${VAR})
	// and is resolved only when the server connects or starts an OAuth flow.
	ClientSecret        string   `json:"client_secret,omitempty"`
	ClientSecretEnv     string   `json:"client_secret_env,omitempty"`
	Scopes              []string `json:"scopes,omitempty"`
	ClientIDMetadataURL string   `json:"client_id_metadata_url,omitempty"`
	Disabled            bool     `json:"disabled,omitempty"`
}

// SamplingConfig configures MCP sampling behavior for a server.
type SamplingConfig struct {
	// Enabled controls whether sampling is allowed for this server (default: true)
	Enabled *bool `json:"enabled,omitempty"`
	// AutoApprove skips the approval prompt for this server
	AutoApprove bool `json:"auto_approve,omitempty"`
	// Provider overrides the provider used for sampling (e.g., "zen", "anthropic")
	Provider string `json:"provider,omitempty"`
	// Model overrides the model used for sampling
	Model string `json:"model,omitempty"`
	// MaxTokens limits the maximum tokens per sampling request
	MaxTokens int `json:"max_tokens,omitempty"`
}

// IsSamplingEnabled returns whether sampling is enabled for this server.
func (c *SamplingConfig) IsSamplingEnabled() bool {
	if c == nil || c.Enabled == nil {
		return true // Enabled by default
	}
	return *c.Enabled
}

// TransportType returns the effective transport type for this server.
func (c *ServerConfig) TransportType() string {
	if c.Type == "http" || c.URL != "" {
		return "http"
	}
	return "stdio"
}

// Validate checks that the server configuration is valid.
func (c *ServerConfig) Validate() error {
	transport := c.TransportType()
	if transport == "http" {
		if c.URL == "" {
			return fmt.Errorf("http transport requires url")
		}
		if c.Command != "" {
			return fmt.Errorf("cannot specify both url and command")
		}
	} else {
		if c.Command == "" {
			return fmt.Errorf("stdio transport requires command")
		}
		if c.URL != "" {
			return fmt.Errorf("cannot specify both url and command")
		}
	}
	return nil
}

// DefaultConfigPath returns the default path for mcp.json.
func DefaultConfigPath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "term-llm", "mcp.json"), nil
}

// LoadConfig loads the MCP configuration from the default path.
func LoadConfig() (*Config, error) {
	path, err := DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	return LoadConfigFromPath(path)
}

// LoadConfigFromPath loads the MCP configuration from a specific path.
func LoadConfigFromPath(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Servers: make(map[string]ServerConfig)}, nil
		}
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Servers == nil {
		cfg.Servers = make(map[string]ServerConfig)
	}
	return &cfg, nil
}

// Save saves the configuration to the default path.
func (c *Config) Save() error {
	path, err := DefaultConfigPath()
	if err != nil {
		return err
	}
	return c.SaveToPath(path)
}

// SaveToPath atomically saves the configuration to a specific path with private permissions.
func (c *Config) SaveToPath(path string) error {
	// Write through symlinks (e.g. dotfile-managed configs) instead of
	// replacing the link itself with a regular file.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create MCP config directory: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal MCP config: %w", err)
	}
	file, err := os.CreateTemp(dir, ".mcp-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary MCP config: %w", err)
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod temporary MCP config: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temporary MCP config: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temporary MCP config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary MCP config: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace MCP config: %w", err)
	}
	return nil
}

// UpdateConfig applies a mutation to the default MCP config under a process and file lock.
func UpdateConfig(fn func(*Config) error) error {
	path, err := DefaultConfigPath()
	if err != nil {
		return fmt.Errorf("find MCP config: %w", err)
	}
	return UpdateConfigAtPath(path, fn)
}

// UpdateConfigAtPath safely reads, mutates and atomically saves a configuration.
func UpdateConfigAtPath(path string, fn func(*Config) error) (err error) {
	configUpdateMu.Lock()
	defer configUpdateMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create MCP config directory: %w", err)
	}
	unlock, err := filelock.Lock(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock MCP config: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil && err == nil {
			err = fmt.Errorf("unlock MCP config: %w", unlockErr)
		}
	}()
	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		return fmt.Errorf("load MCP config: %w", err)
	}
	if err := fn(cfg); err != nil {
		return err
	}
	if err := cfg.SaveToPath(path); err != nil {
		return fmt.Errorf("save MCP config: %w", err)
	}
	return nil
}

// ServerNames returns a sorted list of configured server names.
func (c *Config) ServerNames() []string {
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AddServer adds or updates a server configuration.
func (c *Config) AddServer(name string, cfg ServerConfig) {
	if c.Servers == nil {
		c.Servers = make(map[string]ServerConfig)
	}
	c.Servers[name] = cfg
}

// RemoveServer removes a server configuration.
func (c *Config) RemoveServer(name string) bool {
	if _, ok := c.Servers[name]; ok {
		delete(c.Servers, name)
		return true
	}
	return false
}
