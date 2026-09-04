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
	serveShellReplayBytes      = 1 << 20
	serveShellHeartbeat        = 8 * time.Second
	serveShellChunkBytes       = 32 << 10
	serveShellInputBytes       = 64 << 10
	serveShellMinCols          = 2
	serveShellMaxCols          = 500
	serveShellMinRows          = 1
	serveShellMaxRows          = 300
	serveShellDrainWait        = 3 * time.Second
	serveShellActivityBytes    = 256 << 10
	serveShellActivitySegments = 1024
	serveShellClaimedRanges    = 1024
	serveShellEventRingSize    = 64
	serveShellQueuedInputBytes = 64 << 10
	serveShellMaxWaiters       = 8
)

var (
	errServeShellUnsupported    = errors.New("interactive shells are unsupported on this platform")
	errServeShellClosed         = errors.New("shell manager closed")
	errServeShellStale          = errors.New("shell generation is stale")
	errServeShellInputQueueFull = errors.New("browser input queue is full while agent command starts")
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

type serveShellContextWriter interface {
	WriteContext(context.Context, []byte) (int, error)
}

type serveShellWriteSource string

const (
	serveShellWriteBrowser       serveShellWriteSource = "browser_input"
	serveShellWriteQueuedBrowser serveShellWriteSource = "queued_browser_input"
	serveShellWriteAgent         serveShellWriteSource = "agent_command"
	serveShellWriteInterrupt     serveShellWriteSource = "agent_interrupt"
	serveShellWriteProbe         serveShellWriteSource = "server_probe"
)

type serveShellCollaborationState string

const (
	serveShellCollaborationOff            serveShellCollaborationState = "off"
	serveShellCollaborationReady          serveShellCollaborationState = "ready"
	serveShellCollaborationAgentRunning   serveShellCollaborationState = "agent_running"
	serveShellCollaborationDesynchronized serveShellCollaborationState = "desynchronized"
)

type serveShellCollaborationSnapshot struct {
	ShellID            string                       `json:"shell_id"`
	Supported          bool                         `json:"supported"`
	ShellToolAvailable bool                         `json:"shell_tool_available"`
	Enabled            bool                         `json:"enabled"`
	State              serveShellCollaborationState `json:"state"`
	Revision           uint64                       `json:"revision"`
	Sequence           uint64                       `json:"sequence"`
	CommandID          string                       `json:"command_id"`
	ToolCallID         string                       `json:"tool_call_id"`
	Reason             string                       `json:"reason"`
}

type serveShellCollaborationEvent struct {
	Type        string                           `json:"-"`
	ShellID     string                           `json:"shell_id"`
	Revision    uint64                           `json:"revision"`
	Sequence    uint64                           `json:"sequence"`
	State       serveShellCollaborationState     `json:"state,omitempty"`
	Enabled     bool                             `json:"enabled,omitempty"`
	CommandID   string                           `json:"command_id,omitempty"`
	ToolCallID  string                           `json:"tool_call_id,omitempty"`
	StartOffset int64                            `json:"start_offset"`
	EndOffset   int64                            `json:"end_offset"`
	ExitCode    *int                             `json:"exit_code,omitempty"`
	ResultKind  string                           `json:"result_kind,omitempty"`
	Reason      string                           `json:"reason,omitempty"`
	Snapshot    *serveShellCollaborationSnapshot `json:"collaboration,omitempty"`
}

type serveShellOutputRange struct{ start, end int64 }
type serveShellActivitySegment struct {
	start int64
	data  []byte
}
type serveShellActivityReservation struct {
	id, shellID string
	start, end  int64
}
type serveShellMarkerWaiter struct {
	nonce       string
	finalMarker byte
	ch          chan serveShellProtocolMarker
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

	generationCtx      context.Context
	generationCancel   context.CancelFunc
	protocol           serveShellProtocolParser
	markerWaiter       *serveShellMarkerWaiter
	protocolClaimStart int64
	protocolEnd        int64
	capture            []byte
	captureLimit       int
	captureTruncated   bool
	captureStart       int64
	captureActive      bool

	injectionGate               bool
	injectionFlushing           bool
	queuedInput                 []byte
	queuedInputErr              error
	interruptWrites             uint64
	browserInputRevision        uint64
	displayRedactionStart       int64
	displayRedactionUntil       int64
	displayRedactionHeld        bool
	displayRedactionMarker      byte
	displayRedactionReplacement []byte

	collaborationEnabled       bool
	collaborationMutation      sync.Mutex
	collaborationState         serveShellCollaborationState
	collaborationRevision      uint64
	collaborationSequence      uint64
	collaborationReason        string
	collaborationSupported     bool
	collaborationCapabilityMsg string
	shellToolAvailable         bool
	commandID                  string
	toolCallID                 string
	commandCancel              context.CancelFunc
	disableRequested           bool
	events                     []serveShellCollaborationEvent

	commandLease chan struct{}
	leaseWaiters int

	activityCursor            int64
	shellResultActivityCursor int64
	activityFloor             int64
	activityReservation       *serveShellActivityReservation
	activitySegments          []serveShellActivitySegment
	activityBytes             int
	claimedRanges             []serveShellOutputRange
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
	generationCtx, generationCancel := context.WithCancel(context.Background())
	lease := make(chan struct{}, 1)
	lease <- struct{}{}
	return &serveShell{
		id: id, sessionID: sessionID, cwd: cwd, lastUsed: time.Now(), changed: make(chan struct{}),
		generationCtx: generationCtx, generationCancel: generationCancel,
		collaborationState: serveShellCollaborationOff, collaborationSupported: true, commandLease: lease,
	}
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
	startOffset := s.nextOffset
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
	markers := s.protocol.Feed(startOffset, data)
	notifyDisplay := s.redactProtocolDisplayLocked(markers)
	waiter := s.markerWaiter
	claimEnd := s.protocolEnd
	for _, marker := range markers {
		if waiter != nil && marker.Nonce == waiter.nonce && !marker.Malformed {
			if marker.Kind == waiter.finalMarker {
				claimEnd = marker.End
				s.protocolEnd = marker.End
			}
		}
	}
	if waiter == nil {
		s.appendActivityOutputLocked(startOffset, data)
	} else {
		claimStart := s.protocolClaimStart
		chunkEnd := startOffset + int64(len(data))
		if startOffset < claimStart {
			prefixEnd := min64(chunkEnd, claimStart)
			s.appendActivityOutputLocked(startOffset, data[:prefixEnd-startOffset])
		}
		if claimEnd > 0 && claimEnd < chunkEnd {
			suffixStart := max64(claimEnd, startOffset)
			s.appendActivityOutputLocked(suffixStart, data[suffixStart-startOffset:])
		}
	}
	s.captureOutputLocked(startOffset, data, markers)
	flushGate := false
	for _, marker := range markers {
		if waiter != nil && marker.Nonce == waiter.nonce && marker.Kind == 'B' && !marker.Malformed && s.injectionGate {
			s.injectionGate = false
			s.injectionFlushing = true
			flushGate = true
		}
	}
	if notifyDisplay {
		s.notifyLocked()
	}
	s.mu.Unlock()
	if waiter != nil {
		for _, marker := range markers {
			if marker.Nonce != waiter.nonce {
				continue
			}
			select {
			case waiter.ch <- marker:
			default:
			}
		}
	}
	if flushGate {
		go s.flushQueuedBrowserInput()
	}
}

func (s *serveShell) watchExit() {
	exit, ok := <-s.process.Done()
	if !ok {
		exit = serveShellExit{Code: -1}
	}
	s.invalidate(exit.Code)
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
	visibleNextOffset := s.visibleNextOffsetLocked()
	reset := offset < s.baseOffset || offset > visibleNextOffset
	if reset {
		offset = s.baseOffset
	}
	available := visibleNextOffset - offset
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
		reset: reset, baseOffset: s.baseOffset, nextOffset: visibleNextOffset,
		dataOffset: offset, data: data, exited: s.exited, exitCode: s.exitCode, changed: s.changed,
	}
}

func (s *serveShell) write(data []byte) error {
	return s.writeFrom(serveShellWriteBrowser, data)
}

func (s *serveShell) resize(cols, rows int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.alive() {
		return io.ErrClosedPipe
	}
	if err := s.process.Resize(cols, rows); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *serveShell) invalidate(exitCode int) {
	s.writeMu.Lock()
	s.mu.Lock()
	if !s.exited {
		s.exited = true
		s.exitCode = exitCode
		s.lastUsed = time.Now()
		s.transitionCollaborationLocked(serveShellCollaborationOff, false, "collaboration", "shell exited")
		if s.commandCancel != nil {
			s.commandCancel()
		}
		s.generationCancel()
		s.releaseProtocolDisplayRedactionLocked()
		s.notifyLocked()
	}
	s.mu.Unlock()
	s.writeMu.Unlock()
}

func (s *serveShell) close() {
	s.invalidate(-1)
	if s.process != nil {
		s.process.Close()
	}
}

type serveShellManager struct {
	ttl    time.Duration
	exists func(string) bool

	mu         sync.Mutex
	shells     map[string]*serveShell
	operations map[string]*sync.Mutex
	closed     bool
	stopCh     chan struct{}
	closeCh    sync.Once
}

func newServeShellManager(ttl time.Duration, exists func(string) bool) *serveShellManager {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	m := &serveShellManager{ttl: ttl, exists: exists, shells: make(map[string]*serveShell), operations: make(map[string]*sync.Mutex), stopCh: make(chan struct{})}
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

	for sessionID, shell := range candidates {
		op := m.sessionOperation(sessionID)
		op.Lock()
		missing := m.exists != nil && !m.exists(sessionID)
		if !missing && now.Sub(shell.idleSince()) <= m.ttl {
			op.Unlock()
			continue
		}
		m.mu.Lock()
		current := m.shells[sessionID] == shell
		m.mu.Unlock()
		if current {
			// Invalidate authority while this generation is still discoverable. A
			// response that pins collaboration in this interval must observe either
			// the live shared generation or its explicit exited state, never a
			// transient missing entry that could be interpreted as local mode.
			shell.invalidate(-1)
			m.mu.Lock()
			if m.shells[sessionID] == shell {
				delete(m.shells, sessionID)
			}
			m.mu.Unlock()
			// Keep this session's operation serialized until the old PTY is closed;
			// otherwise create could publish a second live generation in the gap.
			shell.close()
		}
		op.Unlock()
	}
}

func (m *serveShellManager) sessionOperation(sessionID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[sessionID]
	if op == nil {
		op = &sync.Mutex{}
		m.operations[sessionID] = op
	}
	return op
}

func (m *serveShellManager) create(sessionID, cwd string, cols, rows int) (*serveShell, bool, error) {
	op := m.sessionOperation(sessionID)
	op.Lock()
	defer op.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, false, errServeShellClosed
	}
	current := m.shells[sessionID]
	m.mu.Unlock()
	if current != nil && current.alive() {
		if err := current.resize(cols, rows); err != nil {
			return nil, false, fmt.Errorf("resize attached shell: %w", err)
		}
		return current, false, nil
	}
	if current != nil {
		m.mu.Lock()
		if m.shells[sessionID] == current {
			delete(m.shells, sessionID)
		}
		m.mu.Unlock()
		current.close()
	}
	id, err := newServeShellID()
	if err != nil {
		return nil, false, err
	}
	shell := newServeShell(id, sessionID, cwd)
	process, err := startServeShellProcess(cwd, cols, rows, shell.appendOutput)
	if err != nil {
		shell.generationCancel()
		return nil, false, err
	}
	shell.process = process
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		shell.close()
		return nil, false, errServeShellClosed
	}
	m.shells[sessionID] = shell
	m.mu.Unlock()
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
	op := m.sessionOperation(sessionID)
	op.Lock()
	defer op.Unlock()
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
	m.mu.Unlock()
	shell.invalidate(-1)
	m.mu.Lock()
	if m.shells[sessionID] == shell {
		delete(m.shells, sessionID)
	}
	m.mu.Unlock()
	shell.close()
	return nil
}

func (m *serveShellManager) closeSession(sessionID string) {
	op := m.sessionOperation(sessionID)
	op.Lock()
	defer op.Unlock()
	m.mu.Lock()
	shell := m.shells[sessionID]
	m.mu.Unlock()
	if shell != nil {
		shell.invalidate(-1)
		m.mu.Lock()
		if m.shells[sessionID] == shell {
			delete(m.shells, sessionID)
		}
		m.mu.Unlock()
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
	type shellTarget struct {
		sessionID string
		shell     *serveShell
	}
	targets := make([]shellTarget, 0, len(m.shells))
	for sessionID, shell := range m.shells {
		targets = append(targets, shellTarget{sessionID: sessionID, shell: shell})
	}
	// Keep generations discoverable to the controller until their authority is
	// invalidated. Creation is already blocked by closed. Per-session operation
	// locks prevent shutdown from racing another close/eviction of the same PTY.
	m.mu.Unlock()
	for _, target := range targets {
		op := m.sessionOperation(target.sessionID)
		op.Lock()
		m.mu.Lock()
		current := m.shells[target.sessionID] == target.shell
		m.mu.Unlock()
		if current {
			target.shell.invalidate(-1)
			m.mu.Lock()
			if m.shells[target.sessionID] == target.shell {
				delete(m.shells, target.sessionID)
			}
			m.mu.Unlock()
			target.shell.close()
		}
		op.Unlock()
	}
	m.mu.Lock()
	m.shells = map[string]*serveShell{}
	m.mu.Unlock()
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

type serveShellCollaborationRequest struct {
	ShellID string `json:"shell_id"`
	Enabled bool   `json:"enabled"`
}

type serveShellInterruptRequest struct {
	ShellID   string `json:"shell_id"`
	CommandID string `json:"command_id"`
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
	case "shell/collaboration":
		if r.Method == http.MethodPost {
			s.handleShellCollaboration(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST")
	case "shell/interrupt":
		if r.Method == http.MethodPost {
			s.handleShellInterrupt(w, r, sessionID)
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
	r.Body = http.MaxBytesReader(w, r.Body, serveShellInputBytes*2)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid shell request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
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
	collaboration := s.shellCollaborationSnapshot(r.Context(), sessionID, shell)
	writeJSON(w, status, map[string]any{
		"shell_id": shell.id, "cwd": shell.cwd, "created": created, "state": "running",
		"collaboration": collaboration,
	})
}

func writeServeShellError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errServeShellClosed):
		writeOpenAIError(w, http.StatusServiceUnavailable, "shell_unavailable", "shell service is shutting down")
	case errors.Is(err, errServeShellStale):
		writeOpenAIError(w, http.StatusConflict, "stale_shell", "shell generation is stale")
	case errors.Is(err, errServeShellInputQueueFull):
		writeOpenAIError(w, http.StatusConflict, "shell_input_busy", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeOpenAIError(w, http.StatusConflict, "shell_input_rejected", "shell input could not be delivered")
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
		err = shell.writeFromContext(r.Context(), serveShellWriteBrowser, data)
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
	collaboration := s.shellCollaborationSnapshot(r.Context(), sessionID, shell)
	eventSequence := collaboration.Sequence
	if err := writeShellSSE(stream, "ready", map[string]any{
		"shell_id": shell.id, "cwd": shell.cwd, "offset": offset,
		"base_offset": initial.baseOffset, "next_offset": initial.nextOffset,
		"heartbeat_ms": serveShellHeartbeat.Milliseconds(), "replay_bytes": serveShellReplayBytes,
		"sequence": eventSequence, "collaboration": collaboration,
	}); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(serveShellHeartbeat)
	defer heartbeat.Stop()
	exitSent := false
	for {
		snapshot := shell.snapshot(offset)
		events, overrun, _ := shell.collaborationEventsAfter(eventSequence)
		if overrun {
			collaboration := s.shellCollaborationSnapshot(r.Context(), sessionID, shell)
			if err := writeShellSSE(stream, "collaboration", collaboration); err != nil {
				return
			}
			eventSequence = collaboration.Sequence
			flusher.Flush()
			continue
		}
		if len(events) > 0 {
			for _, event := range events {
				if err := writeShellSSE(stream, event.Type, event); err != nil {
					return
				}
				eventSequence = event.Sequence
			}
			flusher.Flush()
			continue
		}
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
			finalCollaboration := s.shellCollaborationSnapshot(r.Context(), sessionID, shell)
			if err := writeShellSSE(stream, "collaboration", finalCollaboration); err != nil {
				return
			}
			if err := writeShellSSE(stream, "exit", map[string]any{
				"shell_id": shell.id, "offset": snapshot.nextOffset, "exit_code": snapshot.exitCode,
				"collaboration": finalCollaboration,
			}); err != nil {
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
