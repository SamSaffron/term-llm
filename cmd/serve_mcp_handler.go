package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
)

const serveMCPStartupTimeout = 30 * time.Second

type serveMCPServerView struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	Tools      int    `json:"tools"`
}

type serveMCPSessionResponse struct {
	Servers []serveMCPServerView `json:"servers"`
	Enabled []string             `json:"enabled"`
}

type serveMCPSelectionRequest struct {
	Enabled []string `json:"enabled"`
}

type serveMCPError struct {
	status    int
	errorType string
	message   string
}

func (e *serveMCPError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func newServeMCPError(status int, errorType, message string) *serveMCPError {
	return &serveMCPError{status: status, errorType: errorType, message: message}
}

func normalizeMCPSelection(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func mcpSelectionString(names []string) string {
	return strings.Join(normalizeMCPSelection(names), ",")
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	return set
}

func mcpServerNameFromToolName(name string) string {
	server, _, ok := strings.Cut(name, "__")
	if !ok {
		return ""
	}
	return server
}

func (rt *serveRuntime) ensureMCPManagerLocked() error {
	if rt == nil {
		return fmt.Errorf("runtime is unavailable")
	}
	if rt.mcpManager == nil {
		mgr := mcp.NewManager()
		if err := mgr.LoadConfig(); err != nil {
			return fmt.Errorf("failed to load MCP config: %w", err)
		}
		rt.mcpManager = mgr
	}
	if rt.provider != nil {
		rt.mcpManager.SetSamplingProvider(rt.provider, rt.defaultModel, rt.yoloMode)
	}
	return nil
}

func (rt *serveRuntime) mcpStateLocked() serveMCPSessionResponse {
	resp := serveMCPSessionResponse{
		Servers: []serveMCPServerView{},
		Enabled: []string{},
	}
	if rt == nil || rt.mcpManager == nil {
		return resp
	}

	available := rt.mcpManager.AvailableServers()
	availableSet := stringSet(available)
	enabled := normalizeMCPSelection(append(rt.mcpManager.EnabledServers(), parseServerList(rt.mcpSetting)...))
	enabledSet := stringSet(enabled)
	toolCounts := make(map[string]int)
	for _, tool := range rt.mcpManager.AllTools() {
		server := mcpServerNameFromToolName(tool.Name)
		if server != "" {
			toolCounts[server]++
		}
	}

	resp.Enabled = append(resp.Enabled, enabled...)
	for _, name := range available {
		status, err := rt.mcpManager.ServerStatus(name)
		view := serveMCPServerView{
			Name:       name,
			Configured: true,
			Enabled:    enabledSet[name],
			Status:     string(status),
			Tools:      toolCounts[name],
		}
		if err != nil {
			view.Error = err.Error()
		}
		resp.Servers = append(resp.Servers, view)
	}
	for _, name := range parseServerList(rt.mcpSetting) {
		if availableSet[name] {
			continue
		}
		resp.Servers = append(resp.Servers, serveMCPServerView{
			Name:       name,
			Configured: false,
			Enabled:    true,
			Status:     string(mcp.StatusFailed),
			Error:      "MCP server is not configured",
		})
	}
	return resp
}

func (rt *serveRuntime) unregisterMCPServerToolsLocked(serverName string) {
	if rt == nil || rt.engine == nil || rt.engine.Tools() == nil {
		return
	}
	prefix := serverName + "__"
	for _, spec := range rt.engine.Tools().AllSpecs() {
		if strings.HasPrefix(spec.Name, prefix) {
			rt.engine.UnregisterTool(spec.Name)
		}
	}
}

func (rt *serveRuntime) registerMCPToolsForServersLocked(serverNames []string) {
	if rt == nil || rt.engine == nil || rt.engine.Tools() == nil || rt.mcpManager == nil {
		return
	}
	requested := stringSet(serverNames)
	for _, spec := range rt.mcpManager.AllTools() {
		server := mcpServerNameFromToolName(spec.Name)
		if server == "" || !requested[server] {
			continue
		}
		rt.engine.Tools().Register(mcp.NewMCPTool(rt.mcpManager, spec))
	}
}

func waitForMCPServersReady(ctx context.Context, manager *mcp.Manager, names []string) error {
	if manager == nil || len(names) == 0 {
		return nil
	}
	deadline := time.NewTimer(serveMCPStartupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	check := func() (bool, error) {
		var failed []string
		var starting []string
		var stopped []string
		for _, name := range names {
			status, err := manager.ServerStatus(name)
			switch status {
			case mcp.StatusReady:
				// ready
			case mcp.StatusStarting:
				starting = append(starting, name)
			case mcp.StatusFailed:
				msg := "unknown error"
				if err != nil {
					msg = err.Error()
				}
				failed = append(failed, fmt.Sprintf("%s (%s)", name, msg))
			default:
				stopped = append(stopped, name)
			}
		}
		if len(failed) > 0 {
			return false, fmt.Errorf("MCP servers failed to start: %s", strings.Join(failed, "; "))
		}
		if len(stopped) > 0 {
			return false, fmt.Errorf("MCP servers stopped before becoming ready: %s", strings.Join(stopped, ", "))
		}
		return len(starting) == 0, nil
	}

	for {
		ready, err := check()
		if ready || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			var stillStarting []string
			for _, name := range names {
				status, _ := manager.ServerStatus(name)
				if status == mcp.StatusStarting {
					stillStarting = append(stillStarting, name)
				}
			}
			if len(stillStarting) == 0 {
				_, err := check()
				return err
			}
			return fmt.Errorf("timed out waiting for MCP servers to start: %s", strings.Join(stillStarting, ", "))
		case <-ticker.C:
		}
	}
}

func (rt *serveRuntime) applyMCPSelectionLocked(ctx context.Context, requestedNames []string) error {
	if rt == nil {
		return newServeMCPError(http.StatusNotFound, "not_found_error", "session not found")
	}
	requested := normalizeMCPSelection(requestedNames)
	if len(requested) == 0 && rt.mcpManager == nil {
		rt.mcpSetting = ""
		return nil
	}
	if err := rt.ensureMCPManagerLocked(); err != nil {
		return newServeMCPError(http.StatusInternalServerError, "server_error", err.Error())
	}

	requestedSet := stringSet(requested)

	available := rt.mcpManager.AvailableServers()
	availableSet := stringSet(available)
	var missing []string
	for _, name := range requested {
		if !availableSet[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		if len(missing) == 1 {
			return newServeMCPError(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("MCP server %q is not configured", missing[0]))
		}
		return newServeMCPError(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("MCP servers are not configured: %s", strings.Join(missing, ", ")))
	}

	currentlyEnabled := rt.mcpManager.EnabledServers()
	currentSet := stringSet(currentlyEnabled)
	// rt.mcpSetting is the persisted/session selection. Include it so stale
	// registry entries from a server that has since failed/stopped are removed
	// when the user disables that server.
	for _, name := range parseServerList(rt.mcpSetting) {
		if availableSet[name] {
			currentSet[name] = true
		}
	}

	var disableNames []string
	for name := range currentSet {
		if !requestedSet[name] {
			disableNames = append(disableNames, name)
		}
	}
	sort.Strings(disableNames)
	for _, name := range disableNames {
		rt.unregisterMCPServerToolsLocked(name)
		if err := rt.mcpManager.Disable(name); err != nil {
			return newServeMCPError(http.StatusInternalServerError, "server_error", fmt.Sprintf("failed to disable MCP server %q: %v", name, err))
		}
	}

	var enableNames []string
	for _, name := range requested {
		status, _ := rt.mcpManager.ServerStatus(name)
		if status != mcp.StatusReady && status != mcp.StatusStarting {
			enableNames = append(enableNames, name)
		}
	}
	for _, name := range enableNames {
		if err := rt.mcpManager.Enable(ctx, name); err != nil {
			return newServeMCPError(http.StatusInternalServerError, "server_error", fmt.Sprintf("failed to start MCP server %q: %v", name, err))
		}
	}
	if err := waitForMCPServersReady(ctx, rt.mcpManager, requested); err != nil {
		return newServeMCPError(http.StatusInternalServerError, "server_error", err.Error())
	}
	rt.registerMCPToolsForServersLocked(requested)
	rt.mcpSetting = strings.Join(requested, ",")
	return nil
}

func (rt *serveRuntime) mcpSelectionReadyLocked(names []string) bool {
	requested := normalizeMCPSelection(names)
	if len(requested) == 0 {
		return rt == nil || rt.mcpManager == nil || len(rt.mcpManager.EnabledServers()) == 0
	}
	if rt == nil || rt.mcpManager == nil {
		return false
	}
	enabledSet := stringSet(rt.mcpManager.EnabledServers())
	for _, name := range requested {
		if !enabledSet[name] {
			return false
		}
		status, _ := rt.mcpManager.ServerStatus(name)
		if status != mcp.StatusReady {
			return false
		}
	}
	return true
}

func (s *serveServer) applyPersistedMCPSelectionLocked(ctx context.Context, sessionID string, rt *serveRuntime) error {
	if s == nil || s.store == nil || rt == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return nil
	}
	desired := normalizeMCPSelection(parseServerList(sess.MCP))
	current := normalizeMCPSelection(parseServerList(rt.mcpSetting))
	if sameStringSlice(desired, current) && rt.mcpSelectionReadyLocked(desired) {
		return nil
	}
	if err := rt.applyMCPSelectionLocked(ctx, desired); err != nil {
		// Keep the persisted/session selection visible even when restoration fails
		// so the UI can show the requested server and let the user turn it off.
		rt.mcpSetting = strings.Join(desired, ",")
		return err
	}
	return nil
}

func (s *serveServer) ensureRuntimeMCPForSession(ctx context.Context, sessionID string, rt *serveRuntime) error {
	if s == nil || s.store == nil || rt == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if rt.hasActiveRun() || !rt.mu.TryLock() {
		return errServeSessionBusy
	}
	defer rt.mu.Unlock()
	if rt.hasActiveRun() {
		return errServeSessionBusy
	}
	if err := s.applyPersistedMCPSelectionLocked(ctx, sessionID, rt); err != nil {
		log.Printf("[serve] restore MCP selection failed for %s: %v", sessionID, err)
	}
	return nil
}

func runtimeProviderName(rt *serveRuntime) string {
	if rt == nil {
		return "unknown"
	}
	if rt.provider != nil {
		if name := strings.TrimSpace(rt.provider.Name()); name != "" {
			return name
		}
	}
	if key := strings.TrimSpace(rt.providerKey); key != "" {
		return key
	}
	return "unknown"
}

func runtimeModelName(rt *serveRuntime) string {
	if rt == nil {
		return "unknown"
	}
	if model := strings.TrimSpace(rt.defaultModel); model != "" {
		return model
	}
	if rt.sessionMeta != nil {
		if model := strings.TrimSpace(rt.sessionMeta.Model); model != "" {
			return model
		}
	}
	return "unknown"
}

func (s *serveServer) persistSessionMCPSelectionLocked(ctx context.Context, sessionID string, rt *serveRuntime, enabled []string) error {
	if s == nil || s.store == nil || rt == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	mcpSetting := mcpSelectionString(enabled)
	now := time.Now()
	updateRuntimeMeta := func(sess *session.Session) {
		// Do not hydrate sessionMeta for an existing, unloaded session here. Run()
		// uses nil sessionMeta as the signal to load persisted history before
		// appending the next turn; setting it from this settings-only endpoint would
		// make the next run forget the stored transcript.
		if rt.sessionMeta != nil && rt.sessionMeta.ID == sessionID {
			rt.sessionMeta = sess
		}
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session Get failed while updating MCP for %s: %w", sessionID, err)
	}
	if sess == nil {
		sess = &session.Session{
			ID:          sessionID,
			Provider:    runtimeProviderName(rt),
			ProviderKey: strings.TrimSpace(rt.providerKey),
			Model:       runtimeModelName(rt),
			Mode:        session.ModeChat,
			Origin:      session.OriginWeb,
			Agent:       rt.agentName,
			CreatedAt:   now,
			UpdatedAt:   now,
			Search:      rt.search,
			Tools:       rt.toolsSetting,
			MCP:         mcpSetting,
			Status:      session.StatusActive,
		}
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			sess.CWD = cwd
		}
		if err := s.store.Create(ctx, sess); err != nil {
			return fmt.Errorf("session Create failed while updating MCP for %s: %w", sessionID, err)
		}
		// This row was created by this endpoint, so there is no persisted transcript
		// to hydrate and it is safe to remember the metadata on the runtime.
		rt.sessionMeta = sess
		return nil
	}
	if strings.TrimSpace(sess.MCP) == mcpSetting {
		updateRuntimeMeta(sess)
		return nil
	}
	sess.MCP = mcpSetting
	sess.UpdatedAt = now
	if err := s.store.Update(ctx, sess); err != nil {
		return fmt.Errorf("session Update failed while updating MCP for %s: %w", sessionID, err)
	}
	updateRuntimeMeta(sess)
	return nil
}

func (s *serveServer) handleSessionMCP(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session runtime is unavailable")
		return
	}
	rt, err := s.sessionMgr.GetOrCreate(r.Context(), sessionID)
	if err != nil {
		status := http.StatusInternalServerError
		errorType := "server_error"
		if errors.Is(err, errServeSessionBusy) || errors.Is(err, errServeSessionLimitReached) {
			status = http.StatusConflict
			errorType = "conflict_error"
		}
		writeOpenAIError(w, status, errorType, err.Error())
		return
	}
	if rt == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	if rt.hasActiveRun() || !rt.mu.TryLock() {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot change MCP servers while a response is running")
		return
	}
	defer rt.mu.Unlock()
	if rt.hasActiveRun() {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot change MCP servers while a response is running")
		return
	}

	if err := rt.ensureMCPManagerLocked(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	switch r.Method {
	case http.MethodGet:
		if err := s.applyPersistedMCPSelectionLocked(r.Context(), sessionID, rt); err != nil {
			log.Printf("[serve] restore MCP selection failed for %s while rendering MCP modal: %v", sessionID, err)
		}
		writeJSON(w, http.StatusOK, rt.mcpStateLocked())
	case http.MethodPatch:
		var req serveMCPSelectionRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if err := rt.applyMCPSelectionLocked(r.Context(), req.Enabled); err != nil {
			if apiErr, ok := err.(*serveMCPError); ok {
				writeOpenAIError(w, apiErr.status, apiErr.errorType, apiErr.message)
				return
			}
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		if err := s.persistSessionMCPSelectionLocked(r.Context(), sessionID, rt, parseServerList(rt.mcpSetting)); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rt.mcpStateLocked())
	default:
		w.Header().Set("Allow", "GET, PATCH")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}
