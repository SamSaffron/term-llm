package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/mcpsession"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// sessionSocket serves this manager's live MCP servers on a private unix socket
// so shell commands in the same session can call them without spawning new
// server processes (see internal/mcpsession).
type sessionSocket struct {
	path       string
	listener   net.Listener
	server     *http.Server
	unregister func()
}

type sessionSocketState struct {
	mu             sync.Mutex
	sessionID      string
	processDefault bool
	socket         *sessionSocket
	failed         bool
}

// SetSessionID binds the manager to a session and enables its socket. Only the
// shell tool of that exact session is pointed at it. Managers that are neither
// bound nor marked with UseAsProcessSession never listen. Rebinding a manager
// that already listens restarts the socket under the new label.
func (m *Manager) SetSessionID(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	st := &m.sessionSocket
	st.mu.Lock()
	if st.sessionID == sessionID {
		st.mu.Unlock()
		return
	}
	st.sessionID = sessionID
	st.failed = false
	old := st.socket
	st.socket = nil
	st.mu.Unlock()
	old.close()
	m.ensureSessionSocketIfRunning()
}

// UseAsProcessSession marks the manager of a session-less, single-manager
// process (e.g. `ask --no-session`) as the target for shell commands that
// carry no session ID. A manager that is also bound to a session registers
// under that session only.
func (m *Manager) UseAsProcessSession() {
	st := &m.sessionSocket
	st.mu.Lock()
	if st.processDefault {
		st.mu.Unlock()
		return
	}
	st.processDefault = true
	st.failed = false
	if st.socket != nil {
		st.socket.unregister()
		st.socket.unregister = st.registerLocked(st.socket.path)
	}
	st.mu.Unlock()
	m.ensureSessionSocketIfRunning()
}

func (st *sessionSocketState) registerLocked(path string) func() {
	if st.sessionID != "" {
		return mcpsession.Register(st.sessionID, path)
	}
	if st.processDefault {
		return mcpsession.RegisterProcessDefault(path)
	}
	return func() {}
}

func (m *Manager) ensureSessionSocketIfRunning() {
	m.mu.RLock()
	running := len(m.clients) > 0 || len(m.startups) > 0
	m.mu.RUnlock()
	if running {
		m.ensureSessionSocket()
	}
}

// SessionSocketPath returns the live socket path, or "" if none is running.
func (m *Manager) SessionSocketPath() string {
	st := &m.sessionSocket
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.socket == nil {
		return ""
	}
	return st.socket.path
}

// ensureSessionSocket starts the socket on first use. Failures are reported
// once per binding and never block MCP itself.
func (m *Manager) ensureSessionSocket() {
	if mcpsession.Disabled() {
		return
	}
	st := &m.sessionSocket
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.socket != nil || st.failed || (st.sessionID == "" && !st.processDefault) {
		return
	}
	label := st.sessionID
	if label == "" {
		label = fmt.Sprintf("pid%d", os.Getpid())
	}
	mcpsession.SweepStale()
	sock, err := m.startSessionSocket(label)
	if err != nil {
		st.failed = true
		runtimeoutput.Warn("MCP session socket disabled", "error", err)
		return
	}
	sock.unregister = st.registerLocked(sock.path)
	st.socket = sock
}

func (m *Manager) startSessionSocket(label string) (*sessionSocket, error) {
	path, err := mcpsession.NewSocketPath(label)
	if err != nil {
		return nil, err
	}
	ln, err := mcpsession.Listen(path)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/session", m.handleSessionInfo)
	mux.HandleFunc("POST /v1/call", m.handleSessionCall)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			runtimeoutput.Warn("MCP session socket stopped", "socket", path, "error", err)
		}
	}()
	return &sessionSocket{path: path, listener: ln, server: srv, unregister: func() {}}, nil
}

// close unregisters, stops serving and removes the socket. The path is unique
// to this socket, so removal cannot affect another manager's listener.
func (sock *sessionSocket) close() {
	if sock == nil {
		return
	}
	sock.unregister()
	_ = sock.server.Close()
	_ = os.Remove(sock.path)
}

// closeSessionSocket stops serving for this manager (StopAll).
func (m *Manager) closeSessionSocket() {
	st := &m.sessionSocket
	st.mu.Lock()
	sock := st.socket
	st.socket = nil
	st.failed = false
	st.mu.Unlock()
	sock.close()
}

func (m *Manager) handleSessionInfo(w http.ResponseWriter, r *http.Request) {
	st := &m.sessionSocket
	st.mu.Lock()
	sessionID := st.sessionID
	st.mu.Unlock()

	info := mcpsession.SessionInfo{SessionID: sessionID, PID: os.Getpid(), Servers: []mcpsession.ServerInfo{}}
	toolsByServer := map[string][]mcpsession.ToolInfo{}
	if snapshot := m.CatalogueSnapshot(); snapshot != nil {
		for _, tool := range snapshot.Tools {
			toolsByServer[tool.Server] = append(toolsByServer[tool.Server], mcpsession.ToolInfo{
				Name:         tool.OriginalName,
				Description:  tool.Description,
				InputSchema:  tool.InputSchema,
				OutputSchema: tool.OutputSchema,
			})
		}
	}
	for _, state := range m.GetAllStates() {
		si := mcpsession.ServerInfo{Name: state.Name, Status: string(state.Status), Tools: toolsByServer[state.Name]}
		if state.Error != nil {
			si.Error = state.Error.Error()
		}
		info.Servers = append(info.Servers, si)
	}
	sort.Slice(info.Servers, func(i, j int) bool { return info.Servers[i].Name < info.Servers[j].Name })
	mcpsession.WriteJSON(w, http.StatusOK, info)
}

func (m *Manager) handleSessionCall(w http.ResponseWriter, r *http.Request) {
	var req mcpsession.CallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		mcpsession.WriteError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if req.Server == "" || req.Tool == "" {
		mcpsession.WriteError(w, http.StatusBadRequest, "server and tool are required")
		return
	}
	args := req.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	// Only servers this session already runs are reachable: the socket never
	// starts servers, so it cannot widen what the session's agent could call
	// directly (MCP tools have no per-call approval beyond server enablement).
	out, err := m.CallCatalogTool(r.Context(), req.Server, req.Tool, req.Server+"__"+req.Tool, args)
	if err != nil {
		mcpsession.WriteError(w, http.StatusConflict, err.Error())
		return
	}
	mcpsession.WriteJSON(w, http.StatusOK, mcpsession.CallResult{Content: out.Content, ContentParts: out.ContentParts, IsError: out.IsError})
}
