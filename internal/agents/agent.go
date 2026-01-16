// Package agents provides named configuration bundles for term-llm.
// Agents combine system prompts, tool sets, model preferences, and MCP servers.
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

	// Behavior
	MaxTurns int `yaml:"max_turns,omitempty"`

	// MCP servers to auto-connect
	MCP []MCPConfig `yaml:"mcp,omitempty"`

	// System prompt (loaded from system.md)
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

// MCPConfig specifies an MCP server to connect.
type MCPConfig struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command,omitempty"`
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

	return nil
}
