package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ServerStatus represents the current state of an MCP server.
type ServerStatus string

const (
	StatusStopped  ServerStatus = "stopped"
	StatusStarting ServerStatus = "starting"
	StatusReady    ServerStatus = "ready"
	StatusFailed   ServerStatus = "failed"
)

// ServerState holds the state of a managed MCP server.
type ServerState struct {
	Name   string
	Status ServerStatus
	Error  error
	Client *Client
}

// StatusUpdate is sent when a server's status changes.
type StatusUpdate struct {
	Name   string
	Status ServerStatus
	Error  error
}

// Manager handles MCP server lifecycle and provides tools to LLM.
type Manager struct {
	config   *Config
	clients  map[string]*Client
	statuses map[string]*ServerState
	mu       sync.RWMutex

	// Channel for status updates (optional, for UI notifications)
	statusChan chan StatusUpdate

	// Sampling handler for createMessage requests
	samplingHandler *SamplingHandler
}

// NewManager creates a new MCP manager.
func NewManager() *Manager {
	return &Manager{
		clients:  make(map[string]*Client),
		statuses: make(map[string]*ServerState),
	}
}

// LoadConfig loads the MCP configuration.
func (m *Manager) LoadConfig() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.config = cfg
	m.mu.Unlock()
	return nil
}

// Config returns the current configuration.
func (m *Manager) Config() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// SetStatusChannel sets a channel to receive status updates.
func (m *Manager) SetStatusChannel(ch chan StatusUpdate) {
	m.mu.Lock()
	m.statusChan = ch
	m.mu.Unlock()
}

// SetSamplingProvider configures the provider and model for MCP sampling requests.
// If yoloMode is true, sampling requests are auto-approved without prompting.
func (m *Manager) SetSamplingProvider(provider llm.Provider, model string, yoloMode bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samplingHandler = NewSamplingHandler(provider, model)
	m.samplingHandler.SetYoloMode(yoloMode)
}

// GetSamplingHandler returns the current sampling handler.
func (m *Manager) GetSamplingHandler() *SamplingHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.samplingHandler
}

// sendStatus sends a status update if a channel is configured.
func (m *Manager) sendStatus(name string, status ServerStatus, err error) {
	m.mu.RLock()
	ch := m.statusChan
	m.mu.RUnlock()
	if ch != nil {
		select {
		case ch <- StatusUpdate{Name: name, Status: status, Error: err}:
		default:
			// Don't block if channel is full
		}
	}
}

// AvailableServers returns the names of all configured servers.
func (m *Manager) AvailableServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.config == nil {
		return nil
	}
	return m.config.ServerNames()
}

// EnabledServers returns the names of currently enabled (running or starting) servers.
func (m *Manager) EnabledServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var names []string
	for name, state := range m.statuses {
		if state.Status == StatusStarting || state.Status == StatusReady {
			names = append(names, name)
		}
	}
	return names
}

// ServerStatus returns the current status of a server.
func (m *Manager) ServerStatus(name string) (ServerStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.statuses[name]
	if !ok {
		return StatusStopped, nil
	}
	return state.Status, state.Error
}

// Enable starts an MCP server in the background (non-blocking).
func (m *Manager) Enable(ctx context.Context, name string) error {
	m.mu.Lock()
	if m.config == nil {
		m.mu.Unlock()
		return fmt.Errorf("no MCP configuration loaded")
	}
	serverCfg, ok := m.config.Servers[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown MCP server: %s", name)
	}

	// Check if already running or starting
	if state, ok := m.statuses[name]; ok {
		if state.Status == StatusStarting || state.Status == StatusReady {
			m.mu.Unlock()
			return nil
		}
	}

	// Create client and set status to starting
	client := NewClient(name, serverCfg)

	// Set sampling handler if available
	if m.samplingHandler != nil {
		client.SetSamplingHandler(m.samplingHandler)
		// Register server config with handler for per-server settings
		m.samplingHandler.SetServerConfig(name, serverCfg)
	}

	m.clients[name] = client
	m.statuses[name] = &ServerState{
		Name:   name,
		Status: StatusStarting,
		Client: client,
	}
	m.mu.Unlock()

	m.sendStatus(name, StatusStarting, nil)

	// Start in background
	go func() {
		err := client.Start(ctx)

		m.mu.Lock()
		state, ok := m.statuses[name]
		if !ok {
			m.mu.Unlock()
			client.Stop()
			return
		}
		if err != nil {
			state.Status = StatusFailed
			state.Error = err
		} else {
			state.Status = StatusReady
			state.Error = nil
		}
		m.mu.Unlock()

		m.sendStatus(name, state.Status, err)
	}()

	return nil
}

// Disable stops an MCP server.
func (m *Manager) Disable(name string) error {
	m.mu.Lock()
	client, ok := m.clients[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.clients, name)
	if state, ok := m.statuses[name]; ok {
		state.Status = StatusStopped
		state.Error = nil
		state.Client = nil
	}
	m.mu.Unlock()

	m.sendStatus(name, StatusStopped, nil)

	return client.Stop()
}

// Restart stops and restarts an MCP server.
func (m *Manager) Restart(ctx context.Context, name string) error {
	if err := m.Disable(name); err != nil {
		return err
	}
	return m.Enable(ctx, name)
}

// StopAll stops all running MCP servers.
func (m *Manager) StopAll() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = make(map[string]*Client)
	m.statuses = make(map[string]*ServerState)
	m.mu.Unlock()

	for _, c := range clients {
		c.Stop()
	}
}

// AllTools returns all tools from all running MCP servers.
// Tool names are prefixed with server name to avoid collisions.
func (m *Manager) AllTools() []ToolSpec {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var allTools []ToolSpec
	for name, state := range m.statuses {
		if state.Status != StatusReady || state.Client == nil {
			continue
		}
		for _, tool := range state.Client.Tools() {
			// Prefix tool name with server name for uniqueness
			prefixedTool := ToolSpec{
				Name:        fmt.Sprintf("%s__%s", name, tool.Name),
				Description: fmt.Sprintf("[%s] %s", name, tool.Description),
				Schema:      tool.Schema,
			}
			allTools = append(allTools, prefixedTool)
		}
	}
	return allTools
}

// CallTool routes a tool call to the appropriate MCP server.
// Tool names should be prefixed with "servername__".
func (m *Manager) CallTool(ctx context.Context, fullName string, args json.RawMessage) (string, error) {
	// Parse server name from tool name
	serverName, toolName := parseToolName(fullName)
	if serverName == "" {
		return "", fmt.Errorf("invalid MCP tool name: %s (expected servername__toolname)", fullName)
	}

	m.mu.RLock()
	state, ok := m.statuses[serverName]
	m.mu.RUnlock()

	if !ok || state.Status != StatusReady || state.Client == nil {
		return "", fmt.Errorf("MCP server %s is not running", serverName)
	}

	return state.Client.CallTool(ctx, toolName, args)
}

// parseToolName extracts server name and tool name from prefixed name.
func parseToolName(fullName string) (serverName, toolName string) {
	for i := 0; i < len(fullName)-1; i++ {
		if fullName[i] == '_' && fullName[i+1] == '_' {
			return fullName[:i], fullName[i+2:]
		}
	}
	return "", fullName
}

// GetAllStates returns the current state of all servers (for UI display).
func (m *Manager) GetAllStates() []ServerState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	states := make([]ServerState, 0, len(m.statuses))
	for _, state := range m.statuses {
		states = append(states, ServerState{
			Name:   state.Name,
			Status: state.Status,
			Error:  state.Error,
		})
	}
	return states
}
