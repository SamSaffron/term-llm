package tools

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/pathutil"
)

// ToolConfig holds configuration for the local tool system.
type ToolConfig struct {
	mu *sync.RWMutex `mapstructure:"-"`

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
	AgentDir        string      `mapstructure:"-"`                  // Agent source directory (set at runtime)
	PlanGuidance    bool        `mapstructure:"-"`                  // Add built-in developer guidance only when update_plan is callable
	// BaseDir, when set, is the per-run/session working directory used to
	// resolve relative tool paths and default process-spawn directories. It is
	// deliberately implemented through explicit path resolution / exec.Cmd.Dir;
	// callers must never use process-wide os.Chdir for session binding.
	BaseDir string `mapstructure:"-"`
	// PrimaryWorkspace is set only for an explicit local/session proposal. BaseDir
	// may still be present solely for relative-path resolution; notably, a serve
	// daemon's process CWD must never become even a proposal by implication.
	PrimaryWorkspace string `mapstructure:"-"`
	// RequireExplicitWorkingDir is enabled only for unbound remote/daemon
	// runtimes. With no BaseDir it rejects relative paths and default process
	// execution instead of falling back to the daemon's ambient CWD.
	RequireExplicitWorkingDir bool `mapstructure:"-"`
	// ShellWorkingDir is retained for compatibility with older callers. Prefer
	// BaseDir for new code. When ShellWorkingDir is empty, shell execution falls
	// back to BaseDir. Legacy callers then use process CWD; unbound daemon
	// runtimes set RequireExplicitWorkingDir to reject that ambient fallback.
	ShellWorkingDir string `mapstructure:"-"`
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

var toolConfigMuInit sync.Mutex

func (c *ToolConfig) mutex() *sync.RWMutex {
	if c == nil {
		return nil
	}
	toolConfigMuInit.Lock()
	defer toolConfigMuInit.Unlock()
	if c.mu == nil {
		c.mu = &sync.RWMutex{}
	}
	return c.mu
}

func (c *ToolConfig) baseDirFields() (baseDir, shellDir string, requireExplicit bool) {
	if c == nil {
		return "", "", false
	}
	mu := c.mutex()
	mu.RLock()
	defer mu.RUnlock()
	return c.BaseDir, c.ShellWorkingDir, c.RequireExplicitWorkingDir
}

func (c *ToolConfig) permissionsFields() (baseDir string, readDirs, writeDirs, shellAllow, scriptCommands []string) {
	if c == nil {
		return "", nil, nil, nil, nil
	}
	mu := c.mutex()
	mu.RLock()
	defer mu.RUnlock()
	return c.BaseDir,
		append([]string(nil), c.ReadDirs...),
		append([]string(nil), c.WriteDirs...),
		append([]string(nil), c.ShellAllow...),
		append([]string(nil), c.ScriptCommands...)
}

// UpdateBaseDir updates only the runtime working directory fields guarded by
// the config lock. ApprovalManager separately owns the proposed/confirmed
// primary workspace state and additional capabilities.
func (c *ToolConfig) UpdateBaseDir(dir string) {
	if c == nil {
		return
	}
	mu := c.mutex()
	mu.Lock()
	defer mu.Unlock()
	c.BaseDir = dir
	c.PrimaryWorkspace = dir
	c.ShellWorkingDir = dir
}

// ClearBaseDir removes an explicit runtime/session binding. Legacy callers then
// fall back to process CWD; runtimes requiring an explicit working directory
// remain fail-closed.
func (c *ToolConfig) ClearBaseDir() {
	if c == nil {
		return
	}
	mu := c.mutex()
	mu.Lock()
	c.BaseDir = ""
	c.PrimaryWorkspace = ""
	c.ShellWorkingDir = ""
	mu.Unlock()
}

// BaseDirValue returns the current runtime BaseDir without racing writers.
func (c *ToolConfig) BaseDirValue() string {
	if c == nil {
		return ""
	}
	mu := c.mutex()
	mu.RLock()
	defer mu.RUnlock()
	return c.BaseDir
}

// PrimaryWorkspaceValue returns the explicitly proposed primary workspace.
func (c *ToolConfig) PrimaryWorkspaceValue() string {
	if c == nil {
		return ""
	}
	mu := c.mutex()
	mu.RLock()
	defer mu.RUnlock()
	return c.PrimaryWorkspace
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

	if other.AgentDir != "" {
		result.AgentDir = other.AgentDir
	}
	if other.PlanGuidance {
		result.PlanGuidance = true
	}
	if other.BaseDir != "" {
		result.BaseDir = other.BaseDir
	}
	if other.PrimaryWorkspace != "" {
		result.PrimaryWorkspace = other.PrimaryWorkspace
	}
	if other.RequireExplicitWorkingDir {
		result.RequireExplicitWorkingDir = true
	}
	if other.ShellWorkingDir != "" {
		result.ShellWorkingDir = other.ShellWorkingDir
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
	if len(other.Spawn.AgentModels) > 0 {
		result.Spawn.AgentModels = other.Spawn.AgentModels
	}

	return result
}

// Validate checks the configuration for errors.
func (c *ToolConfig) Validate() []error {
	var errs []error
	if c == nil {
		return errs
	}
	mu := c.mutex()
	mu.RLock()
	baseDir := c.BaseDir
	requireExplicitWorkingDir := c.RequireExplicitWorkingDir
	enabled := append([]string(nil), c.Enabled...)
	readDirs := append([]string(nil), c.ReadDirs...)
	writeDirs := append([]string(nil), c.WriteDirs...)
	shellAllow := append([]string(nil), c.ShellAllow...)
	mu.RUnlock()

	// Validate tool names
	for _, name := range enabled {
		if !ValidToolName(name) {
			errs = append(errs, fmt.Errorf("unknown tool: %s", name))
		}
	}

	// Validate shell patterns
	for _, pattern := range shellAllow {
		if err := validateShellApprovalPattern(pattern); err != nil {
			errs = append(errs, fmt.Errorf("invalid shell pattern %q: %w", pattern, err))
		}
	}

	if requireExplicitWorkingDir && strings.TrimSpace(baseDir) == "" {
		for _, dir := range readDirs {
			if !filepath.IsAbs(strings.TrimSpace(dir)) {
				errs = append(errs, fmt.Errorf("read_dir %q must be absolute when no session working directory is bound", dir))
			}
		}
		for _, dir := range writeDirs {
			if !filepath.IsAbs(strings.TrimSpace(dir)) {
				errs = append(errs, fmt.Errorf("write_dir %q must be absolute when no session working directory is bound", dir))
			}
		}
	}

	// Warn for nonexistent directories (may be mounted later)
	for _, dir := range readDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			slog.Warn("read_dir does not exist", "dir", dir)
		}
	}
	for _, dir := range writeDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			slog.Warn("write_dir does not exist", "dir", dir)
		}
	}

	return errs
}

// IsToolEnabled checks if a tool is enabled.
func (c *ToolConfig) IsToolEnabled(specName string) bool {
	if c == nil {
		return false
	}
	mu := c.mutex()
	mu.RLock()
	defer mu.RUnlock()
	for _, name := range c.Enabled {
		if name == specName {
			return true
		}
	}
	return false
}

// EnabledSpecNames returns the spec names for all enabled tools.
func (c *ToolConfig) EnabledSpecNames() []string {
	if c == nil {
		return nil
	}
	mu := c.mutex()
	mu.RLock()
	defer mu.RUnlock()
	// Enabled list now contains spec names directly
	return append([]string(nil), c.Enabled...)
}

// CanAutoRunShell checks if shell commands can be auto-run.
func (c *ToolConfig) CanAutoRunShell() bool {
	if c == nil {
		return false
	}
	mu := c.mutex()
	mu.RLock()
	autoRun := c.ShellAutoRun
	autoRunEnv := c.ShellAutoRunEnv
	mu.RUnlock()
	if !autoRun {
		return false
	}
	if autoRunEnv != "" {
		return os.Getenv(autoRunEnv) == "1"
	}
	return true
}

// CanRunShellNonTTY checks if shell can run in non-TTY mode.
func (c *ToolConfig) CanRunShellNonTTY() bool {
	if c == nil {
		return false
	}
	mu := c.mutex()
	mu.RLock()
	nonTTYEnv := c.ShellNonTTYEnv
	mu.RUnlock()
	if nonTTYEnv != "" {
		return os.Getenv(nonTTYEnv) == "1"
	}
	return false
}

// WorkingDir returns the effective per-run working directory. BaseDir is the
// preferred source. ShellWorkingDir is a legacy shell-only override retained for
// compatibility. If neither is set, legacy callers use process CWD; runtimes
// requiring an explicit directory return empty instead.
func (c *ToolConfig) WorkingDir() string {
	if c == nil {
		return mustGetwd()
	}
	baseDir, shellDir, requireExplicit := c.baseDirFields()
	if dir := strings.TrimSpace(baseDir); dir != "" {
		return cleanAbsDir(dir)
	}
	if dir := strings.TrimSpace(shellDir); dir != "" {
		return cleanAbsDir(dir)
	}
	if requireExplicit {
		return ""
	}
	return mustGetwd()
}

// ShellDir returns the effective shell cwd. It preserves the historical
// ShellWorkingDir override while allowing BaseDir to be the unified default.
func (c *ToolConfig) ShellDir() string {
	if c == nil {
		return mustGetwd()
	}
	baseDir, shellDir, requireExplicit := c.baseDirFields()
	if dir := strings.TrimSpace(shellDir); dir != "" {
		return cleanAbsDir(dir)
	}
	if dir := strings.TrimSpace(baseDir); dir != "" {
		return cleanAbsDir(dir)
	}
	if requireExplicit {
		return ""
	}
	return mustGetwd()
}

// RequiresExplicitWorkingDir reports whether ambient process-CWD fallback is
// forbidden for this runtime.
func (c *ToolConfig) RequiresExplicitWorkingDir() bool {
	if c == nil {
		return false
	}
	mu := c.mutex()
	mu.RLock()
	required := c.RequireExplicitWorkingDir
	mu.RUnlock()
	return required
}

// ResolveDir resolves a user-supplied directory against WorkingDir when it is
// relative. Empty input resolves to WorkingDir.
func (c *ToolConfig) ResolveDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return c.WorkingDir()
	}
	return resolvePathAgainstBase(dir, c.WorkingDir())
}

// ResolvePath resolves a user-supplied file or directory path against
// WorkingDir when it is relative. It does not evaluate symlinks; callers that
// need permission-safe canonical paths should pass the result to resolveToolPath.
func (c *ToolConfig) ResolvePath(path string) string {
	return resolvePathAgainstBase(path, c.WorkingDir())
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Clean(cwd)
}

func cleanAbsDir(dir string) string {
	return resolvePathAgainstBase(dir, "")
}

func resolvePathAgainstBase(path, base string) string {
	expanded, err := pathutil.Expand(path)
	if err != nil {
		expanded = path
	}
	if expanded == "" {
		expanded = "."
	}
	if !filepath.IsAbs(expanded) {
		if base == "" {
			if abs, err := filepath.Abs(expanded); err == nil {
				expanded = abs
			}
		} else {
			expanded = filepath.Join(base, expanded)
		}
	}
	return filepath.Clean(expanded)
}

func optionalToolConfig(configs []*ToolConfig) *ToolConfig {
	if len(configs) == 0 {
		return nil
	}
	return configs[0]
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
		return StandardToolNames()
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
	baseDir, readDirs, writeDirs, shellAllow, scriptCommands := c.permissionsFields()
	baseDir = strings.TrimSpace(baseDir)
	baseAbs := ""

	if baseDir != "" {
		baseAbs = cleanAbsDir(baseDir)
	}

	for _, dir := range readDirs {
		if baseAbs != "" {
			dir = resolvePathAgainstBase(dir, baseAbs)
		}
		if err := perms.AddReadDir(dir); err != nil {
			// Non-fatal: directory may not exist yet
			slog.Warn("failed to add read dir", "dir", dir, "error", err)
		}
	}

	for _, dir := range writeDirs {
		if baseAbs != "" {
			dir = resolvePathAgainstBase(dir, baseAbs)
		}
		if err := perms.AddWriteDir(dir); err != nil {
			slog.Warn("failed to add write dir", "dir", dir, "error", err)
		}
	}

	for _, pattern := range shellAllow {
		if err := perms.AddShellPattern(pattern); err != nil {
			return nil, err
		}
	}

	for _, script := range scriptCommands {
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
