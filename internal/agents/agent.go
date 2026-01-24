// Package agents provides named configuration bundles for term-llm.
// Agents combine system prompts, tool sets, model preferences, and MCP servers.
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"gopkg.in/yaml.v3"
)

// Agent represents a named configuration bundle.
type Agent struct {
	// Metadata
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Model preferences (optional)
	Provider string `yaml:"provider,omitempty"`
	Model    string `yaml:"model,omitempty"`

	// Tool configuration
	Tools ToolsConfig `yaml:"tools,omitempty"`

	// Tool-specific settings
	Shell ShellConfig `yaml:"shell,omitempty"`
	Read  ReadConfig  `yaml:"read,omitempty"`
	Spawn SpawnConfig `yaml:"spawn,omitempty"`

	// Behavior
	MaxTurns int  `yaml:"max_turns,omitempty"`
	Search   bool `yaml:"search,omitempty"` // Enable web search tools

	// DefaultPrompt is used when agent is invoked without a message
	DefaultPrompt string `yaml:"default_prompt,omitempty"`

	// Output specifies where to write agent response (deprecated, use OutputTool + OnComplete)
	// Valid: "" (stdout), "commit_editmsg" (.git/COMMIT_EDITMSG)
	Output string `yaml:"output,omitempty"`

	// OutputTool configures a tool for capturing structured output.
	// When set, a tool with the configured name is dynamically created
	// and added to the agent's enabled tools.
	OutputTool OutputToolConfig `yaml:"output_tool,omitempty"`

	// OnComplete is a shell command to run with captured output piped to stdin.
	// Runs in the git repo root (if in a git repo) or cwd.
	// Replaces the hardcoded "output: commit_editmsg" approach.
	OnComplete string `yaml:"on_complete,omitempty"`

	// Include additional .md files in the system prompt
	// Files are loaded from the agent directory and appended after system.md
	Include []string `yaml:"include,omitempty"`

	// MCP servers to auto-connect
	MCP []MCPConfig `yaml:"mcp,omitempty"`

	// System prompt (loaded from system.md + included files)
	SystemPrompt string `yaml:"-"`

	// Source info
	Source     AgentSource `yaml:"-"`
	SourcePath string      `yaml:"-"`
}

// AgentSource indicates where an agent was loaded from.
type AgentSource int

const (
	SourceLocal   AgentSource = iota // Project-local (./term-llm-agents/)
	SourceUser                       // User-global (~/.config/term-llm/agents/)
	SourceBuiltin                    // Embedded built-in
)

// SourceName returns a human-readable name for the agent source.
func (s AgentSource) SourceName() string {
	switch s {
	case SourceLocal:
		return "local"
	case SourceUser:
		return "user"
	case SourceBuiltin:
		return "builtin"
	default:
		return "unknown"
	}
}

// ToolsConfig specifies which tools to enable or disable.
type ToolsConfig struct {
	// Enabled is an explicit allow list of tools
	Enabled []string `yaml:"enabled,omitempty"`
	// Disabled is a deny list (all others enabled)
	Disabled []string `yaml:"disabled,omitempty"`
}

// ShellConfig provides shell tool settings.
type ShellConfig struct {
	Allow   []string          `yaml:"allow,omitempty"`
	AutoRun bool              `yaml:"auto_run,omitempty"`
	Scripts map[string]string `yaml:"scripts,omitempty"` // Named scripts (auto-approved)
}

// ReadConfig provides read tool settings.
type ReadConfig struct {
	Dirs []string `yaml:"dirs,omitempty"`
}

// SpawnConfig configures spawn_agent behavior for this agent.
type SpawnConfig struct {
	MaxParallel    int      `yaml:"max_parallel,omitempty"`   // Max concurrent sub-agents (default 3)
	MaxDepth       int      `yaml:"max_depth,omitempty"`      // Max nesting level (default 2)
	DefaultTimeout int      `yaml:"timeout,omitempty"`        // Default timeout in seconds (default 300)
	AllowedAgents  []string `yaml:"allowed_agents,omitempty"` // Optional whitelist of allowed agents
}

// MCPConfig specifies an MCP server to connect.
type MCPConfig struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command,omitempty"`
}

// OutputToolConfig configures a tool for capturing structured output.
type OutputToolConfig struct {
	Name        string `yaml:"name"`        // Tool name (e.g., "set_commit_message")
	Param       string `yaml:"param"`       // Parameter to capture (default: "content")
	Description string `yaml:"description"` // Tool description
}

// IsConfigured returns true if the output tool is configured.
func (c *OutputToolConfig) IsConfigured() bool {
	return c.Name != ""
}

// LoadFromDir loads an agent from a directory containing agent.yaml and optionally system.md.
func LoadFromDir(dir string, source AgentSource) (*Agent, error) {
	agentPath := filepath.Join(dir, "agent.yaml")

	data, err := os.ReadFile(agentPath)
	if err != nil {
		return nil, fmt.Errorf("read agent.yaml: %w", err)
	}

	var agent Agent
	if err := yaml.Unmarshal(data, &agent); err != nil {
		return nil, fmt.Errorf("parse agent.yaml: %w", err)
	}

	// Load system prompt if exists
	systemPath := filepath.Join(dir, "system.md")
	if systemData, err := os.ReadFile(systemPath); err == nil {
		agent.SystemPrompt = string(systemData)
	}

	// Load and append included files
	for _, include := range agent.Include {
		includePath := filepath.Join(dir, include)
		// Security: validate that the resolved path stays within the agent directory
		// to prevent path traversal attacks via "../" in include paths
		absInclude, err := filepath.Abs(includePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to resolve include path %q: %v\n", include, err)
			continue
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to resolve agent directory: %v\n", err)
			continue
		}
		if !strings.HasPrefix(absInclude, absDir+string(filepath.Separator)) && absInclude != absDir {
			fmt.Fprintf(os.Stderr, "warning: include path %q escapes agent directory, skipping\n", include)
			continue
		}
		if includeData, err := os.ReadFile(includePath); err == nil {
			// Add separator and include content
			agent.SystemPrompt += "\n\n---\n\n"
			agent.SystemPrompt += string(includeData)
		} else if !os.IsNotExist(err) {
			// Log non-existence errors (permission issues, etc.)
			fmt.Fprintf(os.Stderr, "warning: failed to read include %q: %v\n", include, err)
		} else {
			// Log missing includes as a debug hint
			fmt.Fprintf(os.Stderr, "warning: agent include file not found: %s\n", includePath)
		}
	}

	// Set source info
	agent.Source = source
	agent.SourcePath = dir

	// Derive name from directory if not set
	if agent.Name == "" {
		agent.Name = filepath.Base(dir)
	}

	return &agent, nil
}

// LoadFromEmbedded loads an agent from embedded filesystem data.
func LoadFromEmbedded(name string, agentYAML, systemMD []byte) (*Agent, error) {
	var agent Agent
	if err := yaml.Unmarshal(agentYAML, &agent); err != nil {
		return nil, fmt.Errorf("parse embedded agent.yaml: %w", err)
	}

	agent.SystemPrompt = string(systemMD)
	agent.Source = SourceBuiltin
	agent.SourcePath = "builtin:" + name

	if agent.Name == "" {
		agent.Name = name
	}

	return &agent, nil
}

// HasEnabledList returns true if the agent uses an explicit enabled list.
func (a *Agent) HasEnabledList() bool {
	return len(a.Tools.Enabled) > 0
}

// HasDisabledList returns true if the agent uses a disabled list.
func (a *Agent) HasDisabledList() bool {
	return len(a.Tools.Disabled) > 0
}

// GetEnabledTools returns the list of enabled tools.
// If Enabled is set, returns that list.
// If Disabled is set, returns all tools except disabled ones.
// If neither is set, returns nil (use default).
func (a *Agent) GetEnabledTools(allTools []string) []string {
	if a.HasEnabledList() {
		return a.Tools.Enabled
	}
	if a.HasDisabledList() {
		disabled := make(map[string]bool)
		for _, t := range a.Tools.Disabled {
			disabled[t] = true
		}
		var enabled []string
		for _, t := range allTools {
			if !disabled[t] {
				enabled = append(enabled, t)
			}
		}
		return enabled
	}
	return nil
}

// GetMCPServerNames returns the names of MCP servers to connect.
func (a *Agent) GetMCPServerNames() []string {
	var names []string
	for _, m := range a.MCP {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names
}

// String returns a brief description of the agent.
func (a *Agent) String() string {
	var parts []string
	parts = append(parts, a.Name)
	if a.Description != "" {
		parts = append(parts, "-", a.Description)
	}
	return strings.Join(parts, " ")
}

// Validate checks that the agent configuration is valid.
func (a *Agent) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("agent name is required")
	}

	// Can't have both enabled and disabled lists
	if a.HasEnabledList() && a.HasDisabledList() {
		return fmt.Errorf("cannot specify both tools.enabled and tools.disabled")
	}

	// Validate output field (deprecated, but still supported)
	if a.Output != "" && a.Output != "commit_editmsg" {
		return fmt.Errorf("invalid output: %q (valid: commit_editmsg)", a.Output)
	}

	// Can't use both old output and new output_tool
	if a.Output != "" && a.OutputTool.IsConfigured() {
		return fmt.Errorf("cannot specify both output and output_tool; use output_tool + on_complete instead")
	}

	// Validate output_tool if configured
	if a.OutputTool.IsConfigured() {
		if a.OutputTool.Name == "" {
			return fmt.Errorf("output_tool.name is required when output_tool is configured")
		}
	}

	return nil
}

// Merge applies preferences on top of the agent's existing configuration.
// Non-zero/non-nil values in pref override the agent's defaults.
func (a *Agent) Merge(pref config.AgentPreference) {
	// Model preferences
	if pref.Provider != "" {
		a.Provider = pref.Provider
	}
	if pref.Model != "" {
		a.Model = pref.Model
	}

	// Tool configuration - replace entire lists if set
	if len(pref.ToolsEnabled) > 0 {
		a.Tools.Enabled = pref.ToolsEnabled
		a.Tools.Disabled = nil // Clear disabled if enabled is set
	}
	if len(pref.ToolsDisabled) > 0 {
		a.Tools.Disabled = pref.ToolsDisabled
		a.Tools.Enabled = nil // Clear enabled if disabled is set
	}

	// Shell settings - append allow patterns, replace auto_run
	if len(pref.ShellAllow) > 0 {
		a.Shell.Allow = append(a.Shell.Allow, pref.ShellAllow...)
	}
	if pref.ShellAutoRun != nil {
		a.Shell.AutoRun = *pref.ShellAutoRun
	}

	// Spawn settings
	if pref.SpawnMaxParallel != nil {
		a.Spawn.MaxParallel = *pref.SpawnMaxParallel
	}
	if pref.SpawnMaxDepth != nil {
		a.Spawn.MaxDepth = *pref.SpawnMaxDepth
	}
	if pref.SpawnTimeout != nil {
		a.Spawn.DefaultTimeout = *pref.SpawnTimeout
	}
	if len(pref.SpawnAllowedAgents) > 0 {
		a.Spawn.AllowedAgents = pref.SpawnAllowedAgents
	}

	// Behavior
	if pref.MaxTurns != nil {
		a.MaxTurns = *pref.MaxTurns
	}
	if pref.Search != nil {
		a.Search = *pref.Search
	}
}
