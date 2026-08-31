package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
)

// ServerStatus represents the current state of an MCP server.
type ServerStatus string

const (
	StatusStopped      ServerStatus = "stopped"
	StatusStarting     ServerStatus = "starting"
	StatusReady        ServerStatus = "ready"
	StatusFailed       ServerStatus = "failed"
	StatusAuthRequired ServerStatus = "auth_required"
)

var mcpStartupTimeout = 30 * time.Second

// ServerState holds the state of a managed MCP server.
type ServerState struct {
	Name            string
	Status          ServerStatus
	Error           error
	RefreshError    error
	LastToolRefresh time.Time
	ToolCount       int
	Client          *Client
}

// StatusUpdate is sent when a server's status changes.
type StatusUpdate struct {
	Name   string
	Status ServerStatus
	Error  error
}

type serverStartup struct {
	client        *Client
	startupCancel context.CancelFunc
	processCancel context.CancelFunc
}

// Manager handles MCP server lifecycle and provides tools to LLM.
type Manager struct {
	config            *Config
	clients           map[string]*Client
	statuses          map[string]*ServerState
	startups          map[string]*serverStartup
	catalogues        map[string]*ToolSnapshot
	maxToolsPerServer int
	oauthCoordinator  *mcpoauth.Coordinator
	mu                sync.RWMutex

	aggregate           atomic.Pointer[CatalogueSnapshot]
	aggregateGeneration uint64
	catalogueHandler    func(CatalogueEvent)

	// Channel for status updates (optional, for UI notifications)
	statusChan chan StatusUpdate

	// Sampling handler for createMessage requests
	samplingHandler *SamplingHandler
}

// NewManager creates a new MCP manager.
func NewManager() *Manager {
	manager := &Manager{
		clients:    make(map[string]*Client),
		statuses:   make(map[string]*ServerState),
		startups:   make(map[string]*serverStartup),
		catalogues: make(map[string]*ToolSnapshot),
	}
	manager.aggregate.Store(&CatalogueSnapshot{})
	return manager
}

// NewManagerWithConfig creates a manager from an explicit configuration. It is
// useful for isolated runtimes and deterministic evaluation without touching
// the user's global mcp.json.
func NewManagerWithConfig(cfg *Config) *Manager {
	manager := NewManager()
	if cfg == nil {
		cfg = &Config{Servers: make(map[string]ServerConfig)}
	}
	manager.config = cfg
	return manager
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
	if m.config == nil {
		return nil
	}
	copy := &Config{Servers: make(map[string]ServerConfig, len(m.config.Servers))}
	for name, server := range m.config.Servers {
		serverCopy := server
		serverCopy.AlwaysLoad = append([]string(nil), server.AlwaysLoad...)
		serverCopy.Args = append([]string(nil), server.Args...)
		serverCopy.Headers = cloneStringMap(server.Headers)
		serverCopy.Env = cloneStringMap(server.Env)
		if server.OAuth != nil {
			oauthCopy := *server.OAuth
			oauthCopy.Scopes = append([]string(nil), server.OAuth.Scopes...)
			serverCopy.OAuth = &oauthCopy
		}
		copy.Servers[name] = serverCopy
	}
	return copy
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

// SetCatalogueChangeHandler sets the callback invoked after aggregate immutable
// publication. The callback is never invoked while Manager.mu is held.
func (m *Manager) SetCatalogueChangeHandler(handler func(CatalogueEvent)) {
	m.mu.Lock()
	m.catalogueHandler = handler
	m.mu.Unlock()
}

// CatalogueSnapshot returns the current immutable complete namespaced catalogue.
// Callers must not mutate the returned snapshot or its schema maps.
func (m *Manager) CatalogueSnapshot() *CatalogueSnapshot {
	return m.aggregate.Load()
}

func (m *Manager) publishAggregateLocked() *CatalogueSnapshot {
	m.aggregateGeneration++
	tools := make([]CatalogTool, 0)
	latest := time.Time{}
	for server, snapshot := range m.catalogues {
		if snapshot == nil {
			continue
		}
		if snapshot.FetchedAt.After(latest) {
			latest = snapshot.FetchedAt
		}
		for _, tool := range snapshot.Tools {
			tools = append(tools, namespaceCatalogTool(server, snapshot.NamespaceDescription, tool))
		}
	}
	tools = normalizeCatalogueIdentities(tools)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	hashes := make([]string, 0, len(tools))
	for _, tool := range tools {
		hashes = append(hashes, tool.Name+":"+tool.SchemaHash)
	}
	data, _ := json.Marshal(hashes)
	sum := sha256.Sum256(data)
	snapshot := &CatalogueSnapshot{
		Generation: m.aggregateGeneration,
		FetchedAt:  latest,
		Tools:      tools,
		Hash:       hex.EncodeToString(sum[:]),
	}
	m.aggregate.Store(snapshot)
	return snapshot
}

func (m *Manager) handleCatalogueChange(server string, client *Client, oldSnapshot, newSnapshot *ToolSnapshot, refreshErr error) {
	m.mu.Lock()
	if m.clients[server] != client {
		m.mu.Unlock()
		return
	}
	state := m.statuses[server]
	if refreshErr != nil {
		if state != nil {
			state.RefreshError = refreshErr
		}
		handler := m.catalogueHandler
		snapshot := copyCatalogueSnapshot(m.aggregate.Load())
		m.mu.Unlock()
		if handler != nil {
			handler(CatalogueEvent{Server: server, Snapshot: snapshot, Err: refreshErr})
		}
		return
	}
	m.catalogues[server] = copyToolSnapshot(newSnapshot)
	if state != nil {
		state.RefreshError = nil
		state.LastToolRefresh = newSnapshot.FetchedAt
		state.ToolCount = len(newSnapshot.Tools)
	}
	snapshot := m.publishAggregateLocked()
	handler := m.catalogueHandler
	m.mu.Unlock()
	if handler != nil {
		handler(CatalogueEvent{Server: server, Snapshot: copyCatalogueSnapshot(snapshot)})
	}
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

// SetSamplingYoloMode updates yolo mode for the current MCP sampling handler.
func (m *Manager) SetSamplingYoloMode(enabled bool) {
	if m == nil {
		return
	}
	m.mu.RLock()
	handler := m.samplingHandler
	m.mu.RUnlock()
	if handler != nil {
		handler.SetYoloMode(enabled)
	}
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
	defer m.mu.RUnlock()
	m.sendStatusLocked(name, status, err)
}

// sendStatusLocked preserves transition ordering when the caller already owns
// the manager lock. The channel send is deliberately nonblocking.
func (m *Manager) sendStatusLocked(name string, status ServerStatus, err error) {
	if m.statusChan != nil {
		select {
		case m.statusChan <- StatusUpdate{Name: name, Status: status, Error: err}:
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
		if state.Status == StatusStarting || state.Status == StatusReady || state.Status == StatusAuthRequired {
			names = append(names, name)
		}
	}
	sort.Strings(names)
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
		if state.Status == StatusStarting || state.Status == StatusReady || state.Status == StatusAuthRequired {
			m.mu.Unlock()
			return nil
		}
	}

	// Create client and set status to starting.
	client := NewClient(name, serverCfg)
	client.oauthCoordinator = m.oauthCoordinator
	client.maxToolsPerServer = m.maxToolsPerServer
	client.SetCatalogueChangeHandler(func(oldSnapshot, newSnapshot *ToolSnapshot, err error) {
		m.handleCatalogueChange(name, client, oldSnapshot, newSnapshot, err)
	})

	// Set sampling handler if available
	if m.samplingHandler != nil {
		client.SetSamplingHandler(m.samplingHandler)
		// Register server config with handler for per-server settings
		m.samplingHandler.SetServerConfig(name, serverCfg)
	}

	timeout := mcpStartupTimeout
	startupCtx, startupCancel := context.WithTimeout(ctx, timeout)
	processCtx, processCancel := context.WithCancel(context.Background())
	startup := &serverStartup{client: client, startupCancel: startupCancel, processCancel: processCancel}

	m.clients[name] = client
	m.startups[name] = startup
	m.statuses[name] = &ServerState{
		Name:   name,
		Status: StatusStarting,
		Client: client,
	}
	m.mu.Unlock()

	m.sendStatus(name, StatusStarting, nil)

	// Start in background
	go func() {
		cancelProcessOnStartupDone := context.AfterFunc(startupCtx, processCancel)

		err := client.start(startupCtx, processCtx)
		// Stop the startup expiry hook before cleanup-canceling startupCtx so a
		// successfully ready stdio server keeps its long-lived process context.
		startupExpired := !cancelProcessOnStartupDone()
		startupCancel()
		if startupExpired {
			processCancel()
			if err == nil {
				err = startupCtx.Err()
				if err == nil {
					err = context.Canceled
				}
			}
		}
		if err != nil {
			processCancel()
		}

		m.mu.Lock()
		if currentStartup, ok := m.startups[name]; ok && currentStartup == startup {
			delete(m.startups, name)
		}
		state, ok := m.statuses[name]
		if !ok || state.Client != client {
			m.mu.Unlock()
			processCancel()
			client.Stop()
			return
		}

		status := StatusReady
		var catalogueEvent *CatalogueEvent
		var catalogueHandler func(CatalogueEvent)
		if err != nil {
			if isAuthenticationRequired(err) {
				status = StatusAuthRequired
			} else {
				status = StatusFailed
			}
			state.Error = err
		} else {
			state.Error = nil
			if snapshot := client.ToolSnapshot(); snapshot != nil {
				m.catalogues[name] = snapshot
				state.LastToolRefresh = snapshot.FetchedAt
				state.ToolCount = len(snapshot.Tools)
				aggregate := m.publishAggregateLocked()
				catalogueEvent = &CatalogueEvent{Server: name, Snapshot: copyCatalogueSnapshot(aggregate)}
				catalogueHandler = m.catalogueHandler
			}
		}
		state.Status = status
		m.mu.Unlock()

		m.sendStatus(name, status, err)
		if catalogueEvent != nil && catalogueHandler != nil {
			catalogueHandler(*catalogueEvent)
		}
		if err == nil {
			if session := client.currentSession(); session != nil {
				go m.watchSession(name, client, session)
			}
		}
	}()

	return nil
}

func (m *Manager) watchSession(name string, client *Client, session *sdkmcp.ClientSession) {
	err := session.Wait()
	if err == nil {
		err = fmt.Errorf("MCP server %s session ended", name)
	} else {
		err = fmt.Errorf("MCP server %s session ended: %w", name, err)
	}

	m.mu.RLock()
	state, ok := m.statuses[name]
	current := ok && state.Status == StatusReady && state.Client == client && m.clients[name] == client
	m.mu.RUnlock()
	if !current || !client.clearTerminatedSession(session) {
		return
	}

	m.mu.Lock()
	state, ok = m.statuses[name]
	if !ok || state.Client != client || m.clients[name] != client {
		m.mu.Unlock()
		return
	}
	delete(m.catalogues, name)
	snapshot := m.publishAggregateLocked()
	handler := m.catalogueHandler
	status := StatusFailed
	if isAuthenticationRequired(err) {
		status = StatusAuthRequired
	}
	state.Status = status
	state.Error = err
	state.ToolCount = 0
	m.sendStatusLocked(name, status, err)
	m.mu.Unlock()
	if handler != nil {
		handler(CatalogueEvent{Server: name, Snapshot: copyCatalogueSnapshot(snapshot)})
	}
}

// Disable stops an MCP server.
func (m *Manager) Disable(name string) error {
	m.mu.Lock()
	if startup, ok := m.startups[name]; ok {
		delete(m.startups, name)
		startup.startupCancel()
		startup.processCancel()
	}
	client, ok := m.clients[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.clients, name)
	_, hadCatalogue := m.catalogues[name]
	delete(m.catalogues, name)
	var aggregate *CatalogueSnapshot
	var catalogueHandler func(CatalogueEvent)
	if hadCatalogue {
		aggregate = m.publishAggregateLocked()
		catalogueHandler = m.catalogueHandler
	}
	if state, ok := m.statuses[name]; ok {
		state.Status = StatusStopped
		state.Error = nil
		state.RefreshError = nil
		state.ToolCount = 0
		state.Client = nil
	}
	m.mu.Unlock()

	m.sendStatus(name, StatusStopped, nil)
	if aggregate != nil && catalogueHandler != nil {
		catalogueHandler(CatalogueEvent{Server: name, Snapshot: copyCatalogueSnapshot(aggregate)})
	}

	return client.Stop()
}

// Restart stops and restarts an MCP server.
func (m *Manager) Restart(ctx context.Context, name string) error {
	if err := m.Disable(name); err != nil {
		return err
	}
	return m.Enable(ctx, name)
}

// AuthStatus is safe OAuth account metadata for display surfaces.
type AuthStatus = mcpoauth.AuthStatus

// OAuthStartOptions selects the redirect frontend and whether an existing valid
// grant should be replaced.
type OAuthStartOptions struct {
	RedirectURL   string
	Force         bool
	SkipReconnect bool
}

// AuthStatuses reports account state without starting any MCP server.
func (m *Manager) AuthStatuses() map[string]AuthStatus {
	m.mu.RLock()
	cfg := m.config
	m.mu.RUnlock()
	statuses := make(map[string]AuthStatus)
	if cfg == nil {
		return statuses
	}
	coordinator := m.oauthCoordinatorOrDefault()
	for name, server := range cfg.Servers {
		if server.TransportType() != "http" || !automaticOAuthForServer(server) {
			statuses[name] = AuthStatus{State: mcpoauth.AuthNotNeeded}
			continue
		}
		statuses[name] = coordinator.Status(server.URL)
	}
	return statuses
}

// StartOAuth starts a browser authorization flow for one server. Callers should
// provide their callback URL; the default is only useful for custom native
// callback handlers.
func (m *Manager) StartOAuth(ctx context.Context, name string, startOptions ...OAuthStartOptions) (*mcpoauth.Flow, error) {
	m.mu.RLock()
	if m.config == nil {
		m.mu.RUnlock()
		return nil, fmt.Errorf("no MCP configuration loaded")
	}
	server, ok := m.config.Servers[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown MCP server: %s", name)
	}
	if server.TransportType() != "http" || !automaticOAuthForServer(server) {
		return nil, fmt.Errorf("MCP server %s does not use automatic OAuth", name)
	}
	options := OAuthStartOptions{RedirectURL: "http://127.0.0.1/callback"}
	if len(startOptions) > 0 {
		options = startOptions[0]
	}
	coordinator := m.oauthCoordinatorOrDefault()
	oauthOptions := oauthOptionsForServer(server)
	if server.OAuth != nil && server.OAuth.ClientSecretEnv != "" && oauthOptions.ClientSecret == "" {
		return nil, fmt.Errorf("OAuth client secret environment variable %s is not set", server.OAuth.ClientSecretEnv)
	}
	flow, err := coordinator.Start(ctx, server.URL, oauthOptions, options.RedirectURL, options.Force)
	if err != nil {
		return nil, err
	}
	m.sendStatus(name, StatusAuthRequired, nil)
	if !flow.Created || options.SkipReconnect {
		return flow, nil
	}
	go func(flowID string) {
		flow, waitErr := coordinator.Wait(context.Background(), flowID)
		if waitErr != nil || flow == nil || flow.State != mcpoauth.FlowSucceeded {
			m.sendStatus(name, StatusAuthRequired, waitErr)
			return
		}
		m.mu.RLock()
		state := m.statuses[name]
		selected := state != nil && state.Status != StatusStopped
		m.mu.RUnlock()
		if selected {
			_ = m.Restart(context.Background(), name)
		} else {
			m.sendStatus(name, StatusStopped, nil)
		}
	}(flow.ID)
	return flow, nil
}

func (m *Manager) CancelOAuth(name, flowID string) error {
	m.mu.RLock()
	if m.config == nil {
		m.mu.RUnlock()
		return fmt.Errorf("no MCP configuration loaded")
	}
	server, ok := m.config.Servers[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown MCP server: %s", name)
	}
	if !m.oauthCoordinatorOrDefault().Cancel(server.URL, flowID) {
		return fmt.Errorf("OAuth flow is not pending")
	}
	m.sendStatus(name, StatusAuthRequired, nil)
	return nil
}

func (m *Manager) LogoutOAuth(ctx context.Context, name string, localOnly bool) error {
	m.mu.RLock()
	if m.config == nil {
		m.mu.RUnlock()
		return fmt.Errorf("no MCP configuration loaded")
	}
	server, ok := m.config.Servers[name]
	state := m.statuses[name]
	selected := state != nil && state.Status != StatusStopped
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown MCP server: %s", name)
	}
	if server.TransportType() != "http" || !automaticOAuthForServer(server) {
		return nil
	}
	if err := m.oauthCoordinatorOrDefault().Logout(ctx, server.URL, localOnly); err != nil {
		return err
	}
	if selected {
		return m.Restart(ctx, name)
	}
	m.sendStatus(name, StatusStopped, nil)
	return nil
}

func (m *Manager) oauthCoordinatorOrDefault() *mcpoauth.Coordinator {
	if m.oauthCoordinator != nil {
		return m.oauthCoordinator
	}
	return mcpoauth.DefaultCoordinator()
}

func automaticOAuthForServer(server ServerConfig) bool {
	if server.OAuth != nil && server.OAuth.Disabled {
		return false
	}
	for key := range server.Headers {
		if strings.EqualFold(key, "Authorization") {
			return false
		}
	}
	return true
}

func oauthOptionsForServer(server ServerConfig) mcpoauth.Options {
	options := mcpoauth.Options{}
	if server.OAuth != nil {
		options.ClientID = server.OAuth.ClientID
		options.Scopes = append([]string(nil), server.OAuth.Scopes...)
		options.ScopesConfigured = server.OAuth.Scopes != nil
		options.ClientIDMetadataURL = server.OAuth.ClientIDMetadataURL
		if server.OAuth.ClientSecretEnv != "" {
			options.ClientSecret = os.Getenv(server.OAuth.ClientSecretEnv)
		}
	}
	return options
}

func isAuthenticationRequired(err error) bool {
	return errors.Is(err, mcpoauth.ErrAuthenticationRequired) || errors.Is(err, mcpoauth.ErrRefreshRejected)
}

// StopAll stops all running MCP servers.
func (m *Manager) StopAll() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	for _, startup := range m.startups {
		startup.startupCancel()
		startup.processCancel()
	}
	m.clients = make(map[string]*Client)
	m.statuses = make(map[string]*ServerState)
	m.startups = make(map[string]*serverStartup)
	m.catalogues = make(map[string]*ToolSnapshot)
	snapshot := m.publishAggregateLocked()
	handler := m.catalogueHandler
	m.mu.Unlock()

	for _, c := range clients {
		_ = c.Stop()
	}
	if handler != nil {
		handler(CatalogueEvent{Snapshot: copyCatalogueSnapshot(snapshot)})
	}
}

// AllTools returns a copy-safe legacy projection of the complete namespaced catalogue.
func (m *Manager) AllTools() []ToolSpec {
	snapshot := m.aggregate.Load()
	if snapshot == nil {
		return nil
	}
	allTools := make([]ToolSpec, 0, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		allTools = append(allTools, ToolSpec{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      cloneMap(tool.InputSchema),
		})
	}
	return allTools
}

// CallTool routes a canonical flattened tool name. Exact catalogue identity is
// preferred so collision-safe names never depend on delimiter parsing; the
// historical parser remains as a compatibility fallback for legacy callers.
func (m *Manager) CallTool(ctx context.Context, fullName string, args json.RawMessage) (llm.ToolOutput, error) {
	if snapshot := m.aggregate.Load(); snapshot != nil {
		for _, tool := range snapshot.Tools {
			if tool.Name == fullName {
				return m.callCatalogTool(ctx, tool.Server, tool.OriginalName, fullName, args)
			}
		}
	}
	serverName, toolName := parseToolName(fullName)
	if serverName == "" {
		return llm.ToolOutput{}, fmt.Errorf("invalid MCP tool name: %s (expected servername__toolname)", fullName)
	}
	return m.callCatalogTool(ctx, serverName, toolName, fullName, args)
}

// CallCatalogTool routes an explicitly identified catalogue entry. It is used by
// catalogue wrappers and native namespace calls so server/child identity is never
// inferred from the flattened executable name.
func (m *Manager) CallCatalogTool(ctx context.Context, serverName, toolName, executableName string, args json.RawMessage) (llm.ToolOutput, error) {
	if serverName == "" || toolName == "" {
		return llm.ToolOutput{}, fmt.Errorf("invalid MCP catalogue identity %q/%q", serverName, toolName)
	}
	return m.callCatalogTool(ctx, serverName, toolName, executableName, args)
}

func (m *Manager) callCatalogTool(ctx context.Context, serverName, toolName, displayName string, args json.RawMessage) (llm.ToolOutput, error) {
	m.mu.RLock()
	state, ok := m.statuses[serverName]
	if !ok || state.Status != StatusReady || state.Client == nil {
		m.mu.RUnlock()
		return llm.ToolOutput{}, fmt.Errorf("MCP server %s is not running", serverName)
	}
	snapshot := m.catalogues[serverName]
	if snapshot == nil {
		m.mu.RUnlock()
		return llm.ToolOutput{}, fmt.Errorf("MCP tool %s is unavailable: server catalogue is not ready", displayName)
	}
	if _, ok := snapshot.ByOriginal[toolName]; !ok {
		m.mu.RUnlock()
		return llm.ToolOutput{}, fmt.Errorf("MCP tool %s is no longer available in the current catalogue", displayName)
	}
	client := state.Client
	m.mu.RUnlock()

	return client.CallTool(ctx, toolName, args)
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
			Name:            state.Name,
			Status:          state.Status,
			Error:           state.Error,
			RefreshError:    state.RefreshError,
			LastToolRefresh: state.LastToolRefresh,
			ToolCount:       state.ToolCount,
		})
	}
	return states
}
