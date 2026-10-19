package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

// LoadAgent loads and validates an agent by name or path.
// Returns nil if agentName is empty.
// If agentName contains a path separator, it is loaded directly from the filesystem.
// Otherwise, it is looked up in the agent registry.
func LoadAgent(agentName string, cfg *config.Config) (*agents.Agent, error) {
	if agentName == "" {
		return nil, nil
	}

	// If the value contains a path separator, load directly from filesystem
	if agents.IsAgentPath(agentName) {
		agent, err := agents.LoadFromPath(agentName)
		if err != nil {
			return nil, fmt.Errorf("load agent from path: %w", err)
		}
		if err := agent.Validate(); err != nil {
			return nil, fmt.Errorf("invalid agent at %s: %w", agentName, err)
		}
		// Apply preferences by agent name if configured
		if pref, ok := cfg.Agents.Preferences[agent.Name]; ok {
			agent.Merge(pref)
		}
		return agent, nil
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("create agent registry: %w", err)
	}

	// Apply agent preferences from config
	registry.SetPreferences(cfg.Agents.Preferences)

	agent, err := registry.Get(agentName)
	if err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}

	if err := agent.Validate(); err != nil {
		return nil, fmt.Errorf("invalid agent: %w", err)
	}

	return agent, nil
}

// ListAgentNames returns all available agent names for completions.
func ListAgentNames(cfg *config.Config) ([]string, error) {
	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return nil, err
	}
	return registry.ListNames()
}

// LoadPersistedAgent permits a removed definition without lifting any policy
// already stored on its session. Invalid existing definitions fail closed.
func LoadPersistedAgent(name string, cfg *config.Config) (*agents.Agent, error) {
	agent, err := LoadAgent(name, cfg)
	if err == nil {
		return agent, nil
	}
	if errors.Is(err, agents.ErrNotFound) {
		return nil, nil
	}
	if agents.IsAgentPath(name) {
		if _, statErr := os.Stat(filepath.Join(name, "agent.yaml")); errors.Is(statErr, os.ErrNotExist) {
			return nil, nil
		}
	}
	return nil, err
}
