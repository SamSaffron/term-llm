package mcp

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// PermissionError identifies an MCP server denied by application policy.
type PermissionError struct{ Server, Identity string }

func (e *PermissionError) Error() string {
	if e.Identity == "" {
		return fmt.Sprintf("MCP server %q requires an authorized trusted agent", e.Server)
	}
	return fmt.Sprintf("agent %q is not permitted to use MCP server %q", e.Identity, e.Server)
}

// ManagerOption binds immutable run configuration at construction.
type ManagerOption struct{ policyIdentity string }

// WithPolicyIdentity binds a trusted agents.PolicyIdentity to this manager.
// Each child run must construct its own manager rather than inherit one.
func WithPolicyIdentity(identity string) ManagerOption {
	return ManagerOption{policyIdentity: identity}
}

type managerPolicy struct {
	mu        sync.Mutex
	identity  string
	value     config.MCPConfig
	loaded    bool
	err       error
	stamp     policyFileStamp
	attempted bool
}

type policyFileStamp struct {
	path     string
	size     int64
	modified time.Time
}

func currentPolicyStamp() (policyFileStamp, error) {
	path, err := config.FindConfigFile()
	if err != nil {
		return policyFileStamp{}, err
	}
	stamp := policyFileStamp{path: path}
	if path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return stamp, err
		}
		stamp.size, stamp.modified = info.Size(), info.ModTime()
	}
	return stamp, nil
}

// RefreshPolicy is the single startup/reload path. A failed reload retains the
// last-known-good policy; an initial failure denies all access. Reads happen at
// existing manager access/refresh boundaries, not through a filesystem watcher.
func (m *Manager) RefreshPolicy() error {
	m.policy.mu.Lock()
	stamp, err := currentPolicyStamp()
	if err == nil && m.policy.attempted && stamp == m.policy.stamp {
		loaded, cachedErr := m.policy.loaded, m.policy.err
		m.policy.mu.Unlock()
		if !loaded {
			return fmt.Errorf("load MCP access policy: %w", cachedErr)
		}
		m.revokeDeniedServers()
		return nil
	}
	var next config.MCPConfig
	if err == nil {
		var nonempty bool
		next, nonempty, err = config.LoadMCPPolicyFile(stamp.path)
		if err == nil && m.policy.loaded && len(m.policy.value.Servers) != 0 && !nonempty {
			err = fmt.Errorf("MCP policy file %q is missing or empty during reload", stamp.path)
		}
	}
	if err == nil {
		m.policy.stamp, m.policy.attempted = stamp, true
	} else {
		m.policy.attempted = false
	}
	if err != nil {
		if m.policy.err == nil || m.policy.err.Error() != err.Error() {
			runtimeoutput.Logf("[mcp] retain last-known-good access policy: %v", err)
		}
		m.policy.err = err
		loaded := m.policy.loaded
		m.policy.mu.Unlock()
		if loaded {
			m.revokeDeniedServers()
			return nil
		}
		return fmt.Errorf("load MCP access policy: %w", err)
	}
	m.policy.value, m.policy.loaded, m.policy.err = next, true, nil
	m.policy.mu.Unlock()
	m.revokeDeniedServers()
	return nil
}

func (m *Manager) revokeDeniedServers() {
	// Disable publishes catalogue removal so planners unregister stale tools.
	for _, name := range m.enabledServerNames() {
		if m.policyError(name) != nil {
			if err := m.disable(name, true); err != nil {
				runtimeoutput.Logf("[mcp] revoke %s: %v", name, err)
			}
		}
	}
}

func (m *Manager) policyError(name string) error {
	m.policy.mu.Lock()
	defer m.policy.mu.Unlock()
	if !m.policy.loaded {
		return fmt.Errorf("MCP access policy unavailable: %w", m.policy.err)
	}
	if !m.policy.value.Allows(name, m.policy.identity) {
		return &PermissionError{Server: name, Identity: m.policy.identity}
	}
	return nil
}

// CheckAccess refreshes policy at an explicit enable/tool-call boundary.
func (m *Manager) CheckAccess(name string) error {
	if err := m.RefreshPolicy(); err != nil {
		return err
	}
	return m.policyError(name)
}

// ValidateSelection checks the entire selection before any server starts.
func (m *Manager) ValidateSelection(names []string) error {
	if err := m.RefreshPolicy(); err != nil {
		return err
	}
	for _, name := range names {
		if err := m.policyError(name); err != nil {
			return err
		}
		m.mu.RLock()
		_, exists := m.configServer(name)
		m.mu.RUnlock()
		if !exists {
			return fmt.Errorf("unknown MCP server: %s", name)
		}
	}
	return nil
}

func (m *Manager) configServer(name string) (ServerConfig, bool) {
	if m.config == nil {
		return ServerConfig{}, false
	}
	server, ok := m.config.Servers[name]
	return server, ok
}

// FilterSelection removes denied entries, including orphaned policy entries.
// It does not discard unknown connections: callers can still display missing,
// unrestricted servers so users can repair them.
func (m *Manager) FilterSelection(names []string) []string {
	allowed := make([]string, 0, len(names))
	for _, name := range names {
		if m.policyError(name) == nil {
			allowed = append(allowed, name)
		}
	}
	return allowed
}
