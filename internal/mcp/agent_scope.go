package mcp

import (
	"fmt"
	"strings"
)

// AgentAccessError reports a server restricted to other resolved agent names.
type AgentAccessError struct {
	Server string
	Agents []string
}

func (e *AgentAccessError) Error() string {
	return fmt.Sprintf("MCP server %s is restricted to agents: %s", e.Server, strings.Join(e.Agents, ", "))
}

// CheckAgent permits unrestricted servers and exact, non-empty resolved names.
// An unknown agent identity must be passed as an empty string (fail closed).
func (c ServerConfig) CheckAgent(server, agentName string) error {
	if len(c.Agents) == 0 {
		return nil
	}
	for _, allowed := range c.Agents {
		if agentName != "" && allowed == agentName {
			return nil
		}
	}
	return &AgentAccessError{Server: server, Agents: append([]string(nil), c.Agents...)}
}

func (c ServerConfig) validateAgents() error {
	for i, agent := range c.Agents {
		if strings.TrimSpace(agent) == "" {
			return fmt.Errorf("agents[%d] must be a non-empty string", i)
		}
	}
	return nil
}

// NewManagerForAgent binds a manager to a run's resolved agent identity. The
// identity is immutable; children must get their own manager, never the parent's.
func NewManagerForAgent(agentName string) *Manager {
	m := NewManager()
	m.agentName = agentName
	return m
}

// CheckSelection validates all names before a caller changes any live state.
func (m *Manager) CheckSelection(names []string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, name := range names {
		if m.config == nil {
			return fmt.Errorf("no MCP configuration loaded")
		}
		cfg, ok := m.config.Servers[name]
		if !ok {
			return fmt.Errorf("unknown MCP server: %s", name)
		}
		if err := cfg.CheckAgent(name, m.agentName); err != nil {
			return err
		}
	}
	return nil
}

// FilterSelection drops restricted servers, retaining unknown names so callers
// can still report missing configuration separately.
func (m *Manager) FilterSelection(names []string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if m.config != nil {
			if cfg, ok := m.config.Servers[name]; ok && cfg.CheckAgent(name, m.agentName) != nil {
				continue
			}
		}
		filtered = append(filtered, name)
	}
	return filtered
}

// revokeDisallowed stops servers whose access was revoked by a config reload.
func (m *Manager) revokeDisallowed() {
	for _, name := range m.EnabledServers() {
		if m.CheckSelection([]string{name}) != nil {
			m.Disable(name)
		}
	}
}

// NewManagerWithConfigForAgent is the explicit-config counterpart used by
// config-backed session views and isolated tests.
func NewManagerWithConfigForAgent(cfg *Config, agentName string) *Manager {
	m := NewManagerWithConfig(cfg)
	m.agentName = agentName
	return m
}
