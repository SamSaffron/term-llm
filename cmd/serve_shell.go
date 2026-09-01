package cmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	serveShellReplayBytes = 1 << 20
	serveShellHeartbeat   = 8 * time.Second
	serveShellChunkBytes  = 32 << 10
	serveShellInputBytes  = 64 << 10
	serveShellMinCols     = 2
	serveShellMaxCols     = 500
	serveShellMinRows     = 1
	serveShellMaxRows     = 300
	serveShellDrainWait   = 3 * time.Second
)

var (
	errServeShellUnsupported = errors.New("interactive shells are unsupported on this platform")
	errServeShellClosed      = errors.New("shell manager closed")
	errServeShellStale       = errors.New("shell generation is stale")
)

type serveShellExit struct {
	Code int
	Err  error
}

type serveShellProcess interface {
	Write([]byte) (int, error)
	Resize(cols, rows int) error
	Close()
	Done() <-chan serveShellExit
}

type serveShell struct {
	id        string
	sessionID string
	cwd       string
	process   serveShellProcess

	mu         sync.Mutex
	writeMu    sync.Mutex
	output     []byte
	baseOffset int64
	nextOffset int64
	exited     bool
	exitCode   int
	lastUsed   time.Time
	changed    chan struct{}
}

type serveShellSnapshot struct {
	reset      bool
	baseOffset int64
	nextOffset int64
	dataOffset int64
	data       []byte
	exited     bool
	exitCode   int
	changed    <-chan struct{}
}

func newServeShell(id, sessionID, cwd string) *serveShell {
	return &serveShell{id: id, sessionID: sessionID, cwd: cwd, lastUsed: time.Now(), changed: make(chan struct{})}
}

func (s *serveShell) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *serveShell) appendOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	s.nextOffset += int64(len(data))
	s.lastUsed = time.Now()
	if len(data) >= serveShellReplayBytes {
		s.output = append(s.output[:0], data[len(data)-serveShellReplayBytes:]...)
		s.baseOffset = s.nextOffset - int64(len(s.output))
	} else {
		s.output = append(s.output, data...)
		if overflow := len(s.output) - serveShellReplayBytes; overflow > 0 {
			copy(s.output, s.output[overflow:])
			s.output = s.output[:len(s.output)-overflow]
			s.baseOffset += int64(overflow)
		}
	}
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *serveShell) watchExit() {
	exit, ok := <-s.process.Done()
	if !ok {
		exit = serveShellExit{Code: -1}
	}
	s.mu.Lock()
	if !s.exited {
		s.exited = true
		s.exitCode = exit.Code
		s.lastUsed = time.Now()
		s.notifyLocked()
	}
	s.mu.Unlock()
}

func (s *serveShell) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.exited
}

func (s *serveShell) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *serveShell) idleSince() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsed
}

func (s *serveShell) snapshot(offset int64) serveShellSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
	reset := offset < s.baseOffset || offset > s.nextOffset
	if reset {
		offset = s.baseOffset
	}
	available := s.nextOffset - offset
	if available > serveShellChunkBytes {
		available = serveShellChunkBytes
	}
	start := int(offset - s.baseOffset)
	end := start + int(available)
	var data []byte
	if start >= 0 && end <= len(s.output) && start < end {
		data = append([]byte(nil), s.output[start:end]...)
	}
	return serveShellSnapshot{
		reset: reset, baseOffset: s.baseOffset, nextOffset: s.nextOffset,
		dataOffset: offset, data: data, exited: s.exited, exitCode: s.exitCode, changed: s.changed,
	}
}

func (s *serveShell) write(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.alive() {
		return io.ErrClosedPipe
	}
	for len(data) > 0 {
		n, err := s.process.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	s.touch()
	return nil
}

func (s *serveShell) resize(cols, rows int) error {
	if !s.alive() {
		return io.ErrClosedPipe
	}
	if err := s.process.Resize(cols, rows); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *serveShell) close() {
	if s.process != nil {
		s.process.Close()
	}
	s.mu.Lock()
	if !s.exited {
		s.exited = true
		s.exitCode = -1
		s.lastUsed = time.Now()
		s.notifyLocked()
	}
	s.mu.Unlock()
}

type serveShellManager struct {
	ttl    time.Duration
	exists func(string) bool

	mu      sync.Mutex
	shells  map[string]*serveShell
	closed  bool
	stopCh  chan struct{}
	closeCh sync.Once
}

func newServeShellManager(ttl time.Duration, exists func(string) bool) *serveShellManager {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	m := &serveShellManager{ttl: ttl, exists: exists, shells: make(map[string]*serveShell), stopCh: make(chan struct{})}
	go m.janitor()
	return m
}

func (m *serveShellManager) janitor() {
	interval := m.ttl / 2
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.evictExpired()
		case <-m.stopCh:
			return
		}
	}
}

func (m *serveShellManager) evictExpired() {
	now := time.Now()
	m.mu.Lock()
	candidates := make(map[string]*serveShell, len(m.shells))
	for sessionID, shell := range m.shells {
		candidates[sessionID] = shell
	}
	m.mu.Unlock()

	var stale []*serveShell
	for sessionID, shell := range candidates {
		missing := m.exists != nil && !m.exists(sessionID)
		if !missing && now.Sub(shell.idleSince()) <= m.ttl {
			continue
		}
		m.mu.Lock()
		if m.shells[sessionID] == shell {
			delete(m.shells, sessionID)
			stale = append(stale, shell)
		}
		m.mu.Unlock()
	}
	for _, shell := range stale {
		shell.close()
	}
}

func (m *serveShellManager) create(sessionID, cwd string, cols, rows int) (*serveShell, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, errServeShellClosed
	}
	if current := m.shells[sessionID]; current != nil && current.alive() {
		if err := current.resize(cols, rows); err != nil {
			return nil, false, fmt.Errorf("resize attached shell: %w", err)
		}
		return current, false, nil
	}
	if current := m.shells[sessionID]; current != nil {
		delete(m.shells, sessionID)
		current.close()
	}
	id, err := newServeShellID()
	if err != nil {
		return nil, false, err
	}
	shell := newServeShell(id, sessionID, cwd)
	process, err := startServeShellProcess(cwd, cols, rows, shell.appendOutput)
	if err != nil {
		return nil, false, err
	}
	shell.process = process
	m.shells[sessionID] = shell
	go shell.watchExit()
	return shell, true, nil
}

func newServeShellID() (string, error) {
	var value [18]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate shell ID: %w", err)
	}
	return "sh_" + base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func (m *serveShellManager) get(sessionID, shellID string) (*serveShell, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errServeShellClosed
	}
	shell := m.shells[sessionID]
	if shell == nil || shell.id != shellID {
		return nil, errServeShellStale
	}
	shell.touch()
	return shell, nil
}

func (m *serveShellManager) closeShell(sessionID, shellID string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errServeShellClosed
	}
	shell := m.shells[sessionID]
	if shell == nil || shell.id != shellID {
		m.mu.Unlock()
		return errServeShellStale
	}
	delete(m.shells, sessionID)
	m.mu.Unlock()
	shell.close()
	return nil
}

func (m *serveShellManager) closeSession(sessionID string) {
	m.mu.Lock()
	shell := m.shells[sessionID]
	delete(m.shells, sessionID)
	m.mu.Unlock()
	if shell != nil {
		shell.close()
	}
}

func (m *serveShellManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.closeCh.Do(func() { close(m.stopCh) })
	shells := make([]*serveShell, 0, len(m.shells))
	for _, shell := range m.shells {
		shells = append(shells, shell)
	}
	m.shells = map[string]*serveShell{}
	m.mu.Unlock()
	for _, shell := range shells {
		shell.close()
	}
}

type serveShellCreateRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type serveShellIDRequest struct {
	ShellID string `json:"shell_id"`
}

type serveShellInputRequest struct {
	ShellID string `json:"shell_id"`
	Data    string `json:"data"`
}

type serveShellResizeRequest struct {
	ShellID string `json:"shell_id"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

func validServeShellSize(cols, rows int) bool {
	return cols >= serveShellMinCols && cols <= serveShellMaxCols && rows >= serveShellMinRows && rows <= serveShellMaxRows
}

func (s *serveServer) shellManager() (*serveShellManager, error) {
	s.shellsMu.Lock()
	defer s.shellsMu.Unlock()
	if s.shellsClosed {
		return nil, errServeShellClosed
	}
	if s.shells == nil {
		s.shells = newServeShellManager(30*time.Minute, s.shellSessionExists)
	}
	return s.shells, nil
}

func (s *serveServer) closeSessionShell(sessionID string) {
	s.shellsMu.Lock()
	manager := s.shells
	s.shellsMu.Unlock()
	if manager != nil {
		manager.closeSession(sessionID)
	}
}

func (s *serveServer) closeShellManager() {
	s.shellsMu.Lock()
	if s.shellsClosed {
		s.shellsMu.Unlock()
		return
	}
	s.shellsClosed = true
	manager := s.shells
	s.shells = nil
	s.shellsMu.Unlock()
	if manager != nil {
		manager.Close()
	}
}

func (s *serveServer) shellSessionExists(sessionID string) bool {
	if s == nil || s.store == nil {
		return false
	}
	sess, err := s.store.Get(context.Background(), sessionID)
	return err == nil && sess != nil
}

func (s *serveServer) resolveShellCWD(ctx context.Context, sessionID string) (string, error) {
	if s.store == nil {
		return "", os.ErrNotExist
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if sess == nil {
		return "", os.ErrNotExist
	}
	var binding serveWorkspaceBinding
	if strings.TrimSpace(sess.ProjectID) != "" {
		binding, err = s.resolvePersistedProjectWorkspace(ctx, *sess)
	} else {
		binding, err = s.resolveWorkspace(ctx, serveWorkspaceRequest{
			SessionID: sessionID, FirstPartyUI: true, AllowNoProject: true,
		})
	}
	if err != nil {
		return "", err
	}
	cwd := strings.TrimSpace(binding.RuntimeDir)
	if cwd == "" {
		cwd = strings.TrimSpace(sess.WorktreeDir)
	}
	if cwd == "" {
		cwd = strings.TrimSpace(sess.CWD)
	}
	if cwd == "" {
		cwd = strings.TrimSpace(s.startupDir)
	}
	if cwd == "" {
		return "", errors.New("session workspace is unavailable")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("inspect session workspace: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("session workspace is not a directory")
	}
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve session workspace: %w", err)
	}
	return absolute, nil
}

func sameOriginShellRequest(r *http.Request) bool {
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if fetchSite == "cross-site" {
		return false
	}
	// Direct, Hub-proxied, reverse-Hub, and WebRTC requests all originate as a
	// same-origin browser fetch. Hub transports deliberately replace the backend
	// Host, so the browser-owned Fetch Metadata signal is authoritative there.
	if fetchSite == "same-origin" {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	return origin == scheme+"://"+r.Host
}

func (s *serveServer) handleSessionShell(w http.ResponseWriter, r *http.Request, sessionID, suffix string) {
	if !platformServeShellSupported() || !s.cfg.ui {
		writeOpenAIError(w, http.StatusNotImplemented, "unsupported_error", "interactive shells are unavailable")
		return
	}
	if !sameOriginShellRequest(r) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_origin", "shell requests must come from the first-party UI origin")
		return
	}
	switch suffix {
	case "shell":
		if r.Method == http.MethodPost {
			s.handleShellCreate(w, r, sessionID)
			return
		}
		if r.Method == http.MethodDelete {
			s.handleShellDelete(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST, DELETE")
	case "shell/stream":
		if r.Method == http.MethodGet {
			s.handleShellStream(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "GET")
	case "shell/input":
		if r.Method == http.MethodPost {
			s.handleShellInput(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST")
	case "shell/resize":
		if r.Method == http.MethodPost {
			s.handleShellResize(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST")
	}
	writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
}

func decodeServeShellJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return false
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, serveShellInputBytes*2))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid shell request")
		return false
	}
	return true
}

func (s *serveServer) handleShellCreate(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellCreateRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	if !validServeShellSize(req.Cols, req.Rows) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid terminal size")
		return
	}
	cwd, err := s.resolveShellCWD(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
		} else {
			writeWorkspaceError(w, err)
		}
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, created, err := manager.create(sessionID, cwd, req.Cols, req.Rows)
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"shell_id": shell.id, "cwd": shell.cwd, "created": created, "state": "running"})
}

func writeServeShellError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errServeShellClosed):
		writeOpenAIError(w, http.StatusServiceUnavailable, "shell_unavailable", "shell service is shutting down")
	case errors.Is(err, errServeShellStale):
		writeOpenAIError(w, http.StatusConflict, "stale_shell", "shell generation is stale")
	case errors.Is(err, io.ErrClosedPipe):
		writeOpenAIError(w, http.StatusConflict, "shell_exited", "shell has exited")
	default:
		writeOpenAIError(w, http.StatusInternalServerError, "shell_error", err.Error())
	}
}

func (s *serveServer) handleShellInput(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellInputRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil || len(data) == 0 || len(data) > serveShellInputBytes {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid shell input")
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, err := manager.get(sessionID, strings.TrimSpace(req.ShellID))
	if err == nil {
		err = shell.write(data)
	}
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(data)})
}

func (s *serveServer) handleShellResize(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellResizeRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	if !validServeShellSize(req.Cols, req.Rows) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid terminal size")
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, err := manager.get(sessionID, strings.TrimSpace(req.ShellID))
	if err == nil {
		err = shell.resize(req.Cols, req.Rows)
	}
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *serveServer) handleShellDelete(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellIDRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	if err := manager.closeShell(sessionID, strings.TrimSpace(req.ShellID)); err != nil {
		writeServeShellError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeShellSSE(w io.Writer, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func (s *serveServer) handleShellStream(w http.ResponseWriter, r *http.Request, sessionID string) {
	shellID := strings.TrimSpace(r.URL.Query().Get("shell_id"))
	offset, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("offset")), 10, 64)
	if err != nil || offset < 0 || shellID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "shell_id and a non-negative offset are required")
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, err := manager.get(sessionID, shellID)
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	ctx, cancel := s.contextWithShutdown(r.Context())
	defer cancel()
	stream := newStreamingResponseWriter(w, serveStreamWriteTimeout)
	flusher, ok := stream.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming is unsupported")
		return
	}
	setSSEHeaders(stream)
	stream.Header().Set("Cache-Control", "no-cache, no-transform")
	initial := shell.snapshot(offset)
	if err := writeShellSSE(stream, "ready", map[string]any{
		"shell_id": shell.id, "cwd": shell.cwd, "offset": offset,
		"base_offset": initial.baseOffset, "next_offset": initial.nextOffset,
		"heartbeat_ms": serveShellHeartbeat.Milliseconds(), "replay_bytes": serveShellReplayBytes,
	}); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(serveShellHeartbeat)
	defer heartbeat.Stop()
	exitSent := false
	for {
		snapshot := shell.snapshot(offset)
		if snapshot.reset {
			if err := writeShellSSE(stream, "reset", map[string]any{"offset": snapshot.baseOffset, "reason": "replay_window"}); err != nil {
				return
			}
			offset = snapshot.baseOffset
			flusher.Flush()
			continue
		}
		if len(snapshot.data) > 0 {
			next := snapshot.dataOffset + int64(len(snapshot.data))
			if err := writeShellSSE(stream, "output", map[string]any{
				"offset": snapshot.dataOffset, "next_offset": next,
				"data": base64.StdEncoding.EncodeToString(snapshot.data),
			}); err != nil {
				return
			}
			offset = next
			flusher.Flush()
			continue
		}
		if snapshot.exited && !exitSent {
			if err := writeShellSSE(stream, "exit", map[string]any{"offset": snapshot.nextOffset, "exit_code": snapshot.exitCode}); err != nil {
				return
			}
			flusher.Flush()
			exitSent = true
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-snapshot.changed:
		case <-heartbeat.C:
			if _, err := io.WriteString(stream, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
