package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/samsaffron/term-llm/internal/config"
)

// Registry manages agent discovery and resolution.
type Registry struct {
	// Search paths in priority order (first match wins)
	searchPaths []searchPath

	// Whether to include built-in agents
	useBuiltin bool

	// Cache of discovered agents (name -> agent)
	cache map[string]*Agent

	// Preferences to apply on top of agent configs
	preferences map[string]config.AgentPreference
}

type searchPath struct {
	path   string
	source AgentSource
}

// RegistryConfig configures the agent registry.
type RegistryConfig struct {
	UseBuiltin  bool
	SearchPaths []string
}

// NewRegistry creates an agent registry with standard paths.
func NewRegistry(cfg RegistryConfig) (*Registry, error) {
	r := &Registry{
		useBuiltin: cfg.UseBuiltin,
		cache:      make(map[string]*Agent),
	}

	// 1. Project-local agents (./term-llm-agents/)
	cwd, err := os.Getwd()
	if err == nil {
		localDir := filepath.Join(cwd, "term-llm-agents")
		r.searchPaths = append(r.searchPaths, searchPath{
			path:   localDir,
			source: SourceLocal,
		})
	}

	// 2. User-global agents (~/.config/term-llm/agents/)
	if home, err := os.UserHomeDir(); err == nil {
		configDir := os.Getenv("XDG_CONFIG_HOME")
		if configDir == "" {
			configDir = filepath.Join(home, ".config")
		}
		userDir := filepath.Join(configDir, "term-llm", "agents")
		r.searchPaths = append(r.searchPaths, searchPath{
			path:   userDir,
			source: SourceUser,
		})
	}

	// 3. Additional search paths from config
	for _, p := range cfg.SearchPaths {
		r.searchPaths = append(r.searchPaths, searchPath{
			path:   p,
			source: SourceUser, // Treat custom paths as user-level
		})
	}

	return r, nil
}

// SetPreferences sets the preference overrides to apply to agents.
// Call this after creating the registry to configure agent preferences.
// This also invalidates the cache for any agents that have preferences set,
// ensuring the new preferences are applied on next Get().
func (r *Registry) SetPreferences(prefs map[string]config.AgentPreference) {
	// Invalidate cache for agents with changed preferences
	if r.preferences != nil || prefs != nil {
		for name := range r.cache {
			// Check if this agent has preferences in either old or new set
			_, hadPref := r.preferences[name]
			_, hasPref := prefs[name]
			if hadPref || hasPref {
				delete(r.cache, name)
			}
		}
	}
	r.preferences = prefs
}

// Get retrieves an agent by name.
// Resolution order: local > user > search paths > builtin
// Preferences are applied on top of the loaded agent config.
func (r *Registry) Get(name string) (*Agent, error) {
	// Check cache first
	if agent, ok := r.cache[name]; ok {
		return agent, nil
	}

	var agent *Agent
	var err error

	// Search filesystem paths
	for _, sp := range r.searchPaths {
		agentDir := filepath.Join(sp.path, name)
		if isAgentDir(agentDir) {
			agent, err = LoadFromDir(agentDir, sp.source)
			if err != nil {
				return nil, fmt.Errorf("load agent %s: %w", name, err)
			}
			break
		}
	}

	// Check built-in agents if not found in filesystem
	if agent == nil && r.useBuiltin {
		agent, err = getBuiltinAgent(name)
		if err != nil {
			return nil, fmt.Errorf("agent not found: %s", name)
		}
	}

	if agent == nil {
		return nil, fmt.Errorf("agent not found: %s", name)
	}

	// Apply preferences on top of agent config
	if r.preferences != nil {
		if pref, ok := r.preferences[name]; ok {
			agent.Merge(pref)
		}
	}

	r.cache[name] = agent
	return agent, nil
}

// List returns all available agents.
// Each agent appears only once, with first-found taking precedence.
func (r *Registry) List() ([]*Agent, error) {
	seen := make(map[string]bool)
	var agents []*Agent

	// Scan filesystem paths
	for _, sp := range r.searchPaths {
		found, err := r.scanDir(sp.path, sp.source)
		if err != nil {
			continue // Skip directories that don't exist or can't be read
		}
		for _, agent := range found {
			if !seen[agent.Name] {
				seen[agent.Name] = true
				agents = append(agents, agent)
			}
		}
	}

	// Add built-in agents (not shadowed by user agents)
	if r.useBuiltin {
		for _, agent := range getBuiltinAgents() {
			if !seen[agent.Name] {
				seen[agent.Name] = true
				agents = append(agents, agent)
			}
		}
	}

	// Sort by name
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Name < agents[j].Name
	})

	return agents, nil
}

// ListBySource returns agents from a specific source.
func (r *Registry) ListBySource(source AgentSource) ([]*Agent, error) {
	all, err := r.List()
	if err != nil {
		return nil, err
	}

	var filtered []*Agent
	for _, agent := range all {
		if agent.Source == source {
			filtered = append(filtered, agent)
		}
	}
	return filtered, nil
}

// scanDir scans a directory for agent subdirectories.
func (r *Registry) scanDir(dir string, source AgentSource) ([]*Agent, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var agents []*Agent
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		agentDir := filepath.Join(dir, entry.Name())
		if !isAgentDir(agentDir) {
			continue
		}

		agent, err := LoadFromDir(agentDir, source)
		if err != nil {
			// Skip invalid agents
			continue
		}
		agents = append(agents, agent)
	}

	return agents, nil
}

// isAgentDir checks if a directory contains an agent.yaml file.
func isAgentDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "agent.yaml"))
	return err == nil && !info.IsDir()
}

// GetUserAgentsDir returns the path for user-global agents.
func GetUserAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(home, ".config")
	}

	return filepath.Join(configDir, "term-llm", "agents"), nil
}

// GetLocalAgentsDir returns the path for project-local agents.
func GetLocalAgentsDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, "term-llm-agents"), nil
}

// CreateAgentDir creates an agent directory with template files.
func CreateAgentDir(baseDir, name string) error {
	agentDir := filepath.Join(baseDir, name)

	// Create directory
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Create agent.yaml
	agentYAML := fmt.Sprintf(`name: %s
description: "Description of what this agent does"

# Model preferences (optional)
# provider: anthropic
# model: claude-sonnet-4-5

# Tool configuration
tools:
  enabled: [read, glob, grep]    # Explicit allow list
  # OR
  # disabled: [write, shell]     # Deny list (all others enabled)

# Tool-specific settings
# shell:
#   allow: ["git *"]
#   auto_run: false
# read:
#   dirs: ["."]

# Behavior
# max_turns: 10

# MCP servers to auto-connect
# mcp:
#   - name: filesystem
`, name)

	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0644); err != nil {
		return fmt.Errorf("write agent.yaml: %w", err)
	}

	// Create system.md
	systemMD := fmt.Sprintf(`You are a helpful assistant for the {{git_repo}} project.

Today is {{date}}. Working directory: {{cwd}}

## Your Role

Describe the agent's purpose and behavior here.

## Guidelines

- Add specific instructions for this agent
- Include any domain-specific knowledge
- Define output format expectations
`)

	if err := os.WriteFile(filepath.Join(agentDir, "system.md"), []byte(systemMD), 0644); err != nil {
		return fmt.Errorf("write system.md: %w", err)
	}

	return nil
}

// CopyAgent copies an agent to a new location.
func CopyAgent(src *Agent, destDir, newName string) error {
	destAgentDir := filepath.Join(destDir, newName)

	// Create directory
	if err := os.MkdirAll(destAgentDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// If source is from filesystem, copy files directly
	if src.Source != SourceBuiltin && src.SourcePath != "" {
		// Copy agent.yaml
		srcYAML := filepath.Join(src.SourcePath, "agent.yaml")
		if data, err := os.ReadFile(srcYAML); err == nil {
			if err := os.WriteFile(filepath.Join(destAgentDir, "agent.yaml"), data, 0644); err != nil {
				return fmt.Errorf("write agent.yaml: %w", err)
			}
		}

		// Copy system.md if exists
		srcMD := filepath.Join(src.SourcePath, "system.md")
		if data, err := os.ReadFile(srcMD); err == nil {
			if err := os.WriteFile(filepath.Join(destAgentDir, "system.md"), data, 0644); err != nil {
				return fmt.Errorf("write system.md: %w", err)
			}
		}
	} else {
		// For built-in agents, get the embedded content
		if err := copyBuiltinAgent(src.Name, destAgentDir, newName); err != nil {
			return err
		}
	}

	return nil
}
