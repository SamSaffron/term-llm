// Package agents provides named configuration bundles for term-llm.
// Agents combine system prompts, tool sets, model preferences, and MCP servers.
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"gopkg.in/yaml.v3"
)

// validCustomToolNameRE matches valid custom tool names: lowercase letter, then lowercase alphanumeric or underscore.
var validCustomToolNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

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

	// ProjectInstructions controls auto-loading of AGENTS.md, CLAUDE.md, etc.
	// Values: "auto" (default), "true", "false"
	// "auto" includes project instructions if the agent has coding tools (write_file, edit_file, shell)
	ProjectInstructions string `yaml:"project_instructions,omitempty"`

	// MCP servers to auto-connect
	MCP []MCPConfig `yaml:"mcp,omitempty"`

	// GistID for syncing with GitHub Gists (set on export/import)
	GistID string `yaml:"gist_id,omitempty"`

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
	// Custom is a list of script-backed custom tools declared in agent.yaml
	Custom []CustomToolDef `yaml:"custom,omitempty"`
}

// CustomToolDef defines a script-backed custom tool declared in agent.yaml.
// The script is resolved relative to the agent directory and invoked with
// the LLM's tool call arguments as JSON on stdin.
type CustomToolDef struct {
	// Name is the tool name shown to the LLM. Must match ^[a-z][a-z0-9_]*$
	// and must not collide with any built-in tool name.
	Name string `yaml:"name"`

	// Description is the tool description passed to the LLM.
	Description string `yaml:"description"`

	// Script is the path to the script, relative to the agent directory.
	// Subdirectories are allowed (e.g. "scripts/job-status.sh").
	Script string `yaml:"script"`

	// Input is a JSON Schema (type: object) for the tool's input parameters.
	// If omitted, the tool accepts no parameters.
	Input map[string]interface{} `yaml:"input,omitempty"`

	// TimeoutSeconds is the execution timeout. Default 30, max 300.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`

	// Env is a map of additional environment variables to set when running the script.
	Env map[string]string `yaml:"env,omitempty"`
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

// IsAgentPath returns true if the value looks like a filesystem path
// rather than an agent name. It checks for forward slashes, backslashes
// (for Windows-style paths, including from WSL/mixed-shell usage),
// and Windows drive-letter prefixes (e.g., C:\).
func IsAgentPath(value string) bool {
	if strings.Contains(value, "/") || strings.Contains(value, `\`) {
		return true
	}
	// Detect Windows drive-letter prefix: C:\ D:\ etc.
	if len(value) >= 3 && value[1] == ':' &&
		((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) {
		return true
	}
	return false
}

// LoadFromPath loads an agent from an explicit filesystem path.
// The path is resolved to absolute. Returns an error if the directory
// doesn't exist or doesn't contain agent.yaml.
func LoadFromPath(path string) (*Agent, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve agent path: %w", err)
	}
	if !isAgentDir(absPath) {
		return nil, fmt.Errorf("not a valid agent directory (missing agent.yaml): %s", absPath)
	}
	return LoadFromDir(absPath, SourceLocal)
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

	// Validate custom tools
	if err := validateCustomTools(a.Tools.Custom); err != nil {
		return err
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

	// Validate project_instructions field
	switch a.ProjectInstructions {
	case "", "auto", "true", "false":
		// Valid values
	default:
		return fmt.Errorf("invalid project_instructions: %q (valid: auto, true, false)", a.ProjectInstructions)
	}

	return nil
}

// ShouldLoadProjectInstructions returns true if this agent should load project instruction files.
// Uses "auto" logic by default: include if agent has coding tools (write_file, edit_file, shell).
func (a *Agent) ShouldLoadProjectInstructions() bool {
	switch a.ProjectInstructions {
	case "true":
		return true
	case "false":
		return false
	default: // "auto" or empty
		return a.hasCodingTools()
	}
}

// hasCodingTools returns true if the agent has tools that modify code.
func (a *Agent) hasCodingTools() bool {
	coding := map[string]bool{
		"write_file": true,
		"edit_file":  true,
		"shell":      true,
	}

	// If enabled list is set, check if any coding tool is in it
	if len(a.Tools.Enabled) > 0 {
		for _, tool := range a.Tools.Enabled {
			if coding[tool] {
				return true
			}
		}
		return false
	}

	// If disabled list is set, coding tools are enabled unless explicitly disabled
	if len(a.Tools.Disabled) > 0 {
		disabled := map[string]bool{}
		for _, tool := range a.Tools.Disabled {
			disabled[tool] = true
		}
		for tool := range coding {
			if !disabled[tool] {
				return true
			}
		}
		return false
	}

	// No tool configuration specified - assume default tool set which includes coding tools
	return true
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

// validateCustomTools checks that a slice of CustomToolDef entries are valid.
// It verifies name format, uniqueness within the list, script path safety,
// and input schema structure. It does NOT check for collisions with built-in
// tool names (that requires the tools package and is done at registration time).
func validateCustomTools(defs []CustomToolDef) error {
	seen := make(map[string]bool)
	for i, def := range defs {
		if def.Name == "" {
			return fmt.Errorf("tools.custom[%d]: name is required", i)
		}
		if !validCustomToolNameRE.MatchString(def.Name) {
			return fmt.Errorf("tools.custom[%d] %q: name must match ^[a-z][a-z0-9_]*$", i, def.Name)
		}
		if seen[def.Name] {
			return fmt.Errorf("tools.custom: duplicate tool name %q", def.Name)
		}
		seen[def.Name] = true

		if def.Description == "" {
			return fmt.Errorf("tools.custom %q: description is required", def.Name)
		}
		if def.Script == "" {
			return fmt.Errorf("tools.custom %q: script is required", def.Name)
		}
		// Script must be a relative path
		if filepath.IsAbs(def.Script) {
			return fmt.Errorf("tools.custom %q: script must be a relative path (got %q)", def.Name, def.Script)
		}
		// Input schema, if provided, must have type: object at root
		if def.Input != nil {
			t, ok := def.Input["type"]
			if !ok || t != "object" {
				return fmt.Errorf("tools.custom %q: input schema must have \"type\": \"object\" at root", def.Name)
			}
		}
	}
	return nil
}
