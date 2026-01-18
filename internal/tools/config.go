package tools

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/gobwas/glob"
)

// ToolConfig holds configuration for the local tool system.
type ToolConfig struct {
	Enabled         []string    `mapstructure:"enabled"`            // Enabled tool spec names
	ReadDirs        []string    `mapstructure:"read_dirs"`          // Directories for read operations
	WriteDirs       []string    `mapstructure:"write_dirs"`         // Directories for write operations
	ShellAllow      []string    `mapstructure:"shell_allow"`        // Shell command patterns
	ScriptCommands  []string    `mapstructure:"script_commands"`    // Exact script commands (auto-approved)
	ShellAutoRun    bool        `mapstructure:"shell_auto_run"`     // Auto-approve matching shell
	ShellAutoRunEnv string      `mapstructure:"shell_auto_run_env"` // Env var required for auto-run
	ShellNonTTYEnv  string      `mapstructure:"shell_non_tty_env"`  // Env var for non-TTY execution
	ImageProvider   string      `mapstructure:"image_provider"`     // Override for image provider
	Spawn           SpawnConfig `mapstructure:"spawn"`              // Spawn agent configuration
}

// DefaultToolConfig returns sensible defaults for tool configuration.
func DefaultToolConfig() ToolConfig {
	return ToolConfig{
		Enabled:         []string{},
		ReadDirs:        []string{},
		WriteDirs:       []string{},
		ShellAllow:      []string{},
		ScriptCommands:  []string{},
		ShellAutoRun:    false,
		ShellAutoRunEnv: "TERM_LLM_ALLOW_AUTORUN",
		ShellNonTTYEnv:  "TERM_LLM_ALLOW_NON_TTY",
		ImageProvider:   "",
		Spawn:           DefaultSpawnConfig(),
	}
}

// Merge combines this config with another, with other taking precedence for non-empty values.
func (c ToolConfig) Merge(other ToolConfig) ToolConfig {
	result := c

	if len(other.Enabled) > 0 {
		result.Enabled = other.Enabled
	}
	if len(other.ReadDirs) > 0 {
		result.ReadDirs = append(result.ReadDirs, other.ReadDirs...)
	}
	if len(other.WriteDirs) > 0 {
		result.WriteDirs = append(result.WriteDirs, other.WriteDirs...)
	}
	if len(other.ShellAllow) > 0 {
		result.ShellAllow = append(result.ShellAllow, other.ShellAllow...)
	}
	if len(other.ScriptCommands) > 0 {
		result.ScriptCommands = append(result.ScriptCommands, other.ScriptCommands...)
	}
	if other.ShellAutoRun {
		result.ShellAutoRun = true
	}
	if other.ShellAutoRunEnv != "" {
		result.ShellAutoRunEnv = other.ShellAutoRunEnv
	}
	if other.ShellNonTTYEnv != "" {
		result.ShellNonTTYEnv = other.ShellNonTTYEnv
	}
	if other.ImageProvider != "" {
		result.ImageProvider = other.ImageProvider
	}

	// Merge spawn config
	if other.Spawn.MaxParallel > 0 {
		result.Spawn.MaxParallel = other.Spawn.MaxParallel
	}
	if other.Spawn.MaxDepth > 0 {
		result.Spawn.MaxDepth = other.Spawn.MaxDepth
	}
	if other.Spawn.DefaultTimeout > 0 {
		result.Spawn.DefaultTimeout = other.Spawn.DefaultTimeout
	}
	if len(other.Spawn.AllowedAgents) > 0 {
		result.Spawn.AllowedAgents = other.Spawn.AllowedAgents
	}

	return result
}

// Validate checks the configuration for errors.
func (c *ToolConfig) Validate() []error {
	var errs []error

	// Validate tool names
	for _, name := range c.Enabled {
		if !ValidToolName(name) {
			errs = append(errs, fmt.Errorf("unknown tool: %s", name))
		}
	}

	// Validate shell patterns
	for _, pattern := range c.ShellAllow {
		if _, err := glob.Compile(pattern); err != nil {
			errs = append(errs, fmt.Errorf("invalid shell pattern %q: %w", pattern, err))
		}
	}

	// Warn for nonexistent directories (may be mounted later)
	for _, dir := range c.ReadDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			slog.Warn("read_dir does not exist", "dir", dir)
		}
	}
	for _, dir := range c.WriteDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			slog.Warn("write_dir does not exist", "dir", dir)
		}
	}

	return errs
}

// IsToolEnabled checks if a tool is enabled.
func (c *ToolConfig) IsToolEnabled(specName string) bool {
	for _, name := range c.Enabled {
		if name == specName {
			return true
		}
	}
	return false
}

// EnabledSpecNames returns the spec names for all enabled tools.
func (c *ToolConfig) EnabledSpecNames() []string {
	// Enabled list now contains spec names directly
	return c.Enabled
}

// CanAutoRunShell checks if shell commands can be auto-run.
func (c *ToolConfig) CanAutoRunShell() bool {
	if !c.ShellAutoRun {
		return false
	}
	if c.ShellAutoRunEnv != "" {
		return os.Getenv(c.ShellAutoRunEnv) == "1"
	}
	return true
}

// CanRunShellNonTTY checks if shell can run in non-TTY mode.
func (c *ToolConfig) CanRunShellNonTTY() bool {
	if c.ShellNonTTYEnv != "" {
		return os.Getenv(c.ShellNonTTYEnv) == "1"
	}
	return false
}

// ParseToolsFlag parses a comma-separated list of tool names.
// Special values: "all" or "*" expand to all available tools.
func ParseToolsFlag(value string) []string {
	if value == "" {
		return nil
	}
	// Handle "all" or "*" to enable all tools
	trimmed := strings.TrimSpace(value)
	if trimmed == "all" || trimmed == "*" {
		return AllToolNames()
	}
	parts := strings.Split(value, ",")
	var tools []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			tools = append(tools, p)
		}
	}
	return tools
}

// BuildPermissions creates a ToolPermissions from this config.
func (c *ToolConfig) BuildPermissions() (*ToolPermissions, error) {
	perms := NewToolPermissions()

	for _, dir := range c.ReadDirs {
		if err := perms.AddReadDir(dir); err != nil {
			// Non-fatal: directory may not exist yet
			slog.Warn("failed to add read dir", "dir", dir, "error", err)
		}
	}

	for _, dir := range c.WriteDirs {
		if err := perms.AddWriteDir(dir); err != nil {
			slog.Warn("failed to add write dir", "dir", dir, "error", err)
		}
	}

	for _, pattern := range c.ShellAllow {
		if err := perms.AddShellPattern(pattern); err != nil {
			return nil, err
		}
	}

	for _, script := range c.ScriptCommands {
		perms.AddScriptCommand(script)
	}

	return perms, nil
}

// NewToolConfigFromFields creates a ToolConfig from individual field values.
// This allows callers from the config package to create ToolConfigs without circular imports.
func NewToolConfigFromFields(enabled, readDirs, writeDirs, shellAllow []string, shellAutoRun bool, shellAutoRunEnv, shellNonTTYEnv, imageProvider string) ToolConfig {
	return ToolConfig{
		Enabled:         enabled,
		ReadDirs:        readDirs,
		WriteDirs:       writeDirs,
		ShellAllow:      shellAllow,
		ShellAutoRun:    shellAutoRun,
		ShellAutoRunEnv: shellAutoRunEnv,
		ShellNonTTYEnv:  shellNonTTYEnv,
		ImageProvider:   imageProvider,
	}
}

// OutputLimits defines limits for tool output.
type OutputLimits struct {
	MaxLines       int   // Max lines for read_file (default 2000)
	MaxBytes       int64 // Max bytes per tool output (default 50KB)
	MaxResults     int   // Max results for grep/glob (default 100/200)
	CumulativeSoft int64 // Soft cumulative limit per turn (default 100KB)
	CumulativeHard int64 // Hard cumulative limit per turn (default 200KB)
}

// DefaultOutputLimits returns the default output limits.
func DefaultOutputLimits() OutputLimits {
	return OutputLimits{
		MaxLines:       2000,
		MaxBytes:       50 * 1024, // 50KB
		MaxResults:     100,
		CumulativeSoft: 100 * 1024, // 100KB
		CumulativeHard: 200 * 1024, // 200KB
	}
}
