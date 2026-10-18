package shell

import (
	"context"
	"errors"
	"github.com/samsaffron/term-llm/internal/app/serve/protocol"
	"io"
	"sync"
	"time"
)

const (
	ReplayBytes      = 1 << 20
	Heartbeat        = 8 * time.Second
	chunkBytes       = 32 << 10
	InputBytes       = 64 << 10
	minCols          = 2
	maxCols          = 500
	minRows          = 1
	maxRows          = 300
	drainWait        = 3 * time.Second
	activityBytes    = 256 << 10
	activitySegments = 1024
	claimedRanges    = 1024
	eventRingSize    = 64
	queuedInputBytes = 64 << 10
	maxWaiters       = 8
)

var (
	errUnsupported    = errors.New("interactive shells are unsupported on this platform")
	ErrClosed         = errors.New("shell manager closed")
	ErrStale          = errors.New("shell generation is stale")
	ErrInputQueueFull = errors.New("browser input queue is full while agent command starts")
)

type Exit struct {
	Code int
	Err  error
}

type Process interface {
	Write([]byte) (int, error)
	Resize(cols, rows int) error
	Close()
	Done() <-chan Exit
}

type contextWriter interface {
	WriteContext(context.Context, []byte) (int, error)
}

type writeSource string

const (
	writeBrowser       writeSource = "browser_input"
	writeQueuedBrowser writeSource = "queued_browser_input"
	writeAgent         writeSource = "agent_command"
	writeInterrupt     writeSource = "agent_interrupt"
	writeProbe         writeSource = "server_probe"
)

type CollaborationState string

const (
	CollaborationOff            CollaborationState = "off"
	CollaborationReady          CollaborationState = "ready"
	CollaborationAgentRunning   CollaborationState = "agent_running"
	CollaborationDesynchronized CollaborationState = "desynchronized"
)

type CollaborationSnapshot struct {
	ShellID            string             `json:"shell_id"`
	Supported          bool               `json:"supported"`
	ShellToolAvailable bool               `json:"shell_tool_available"`
	Enabled            bool               `json:"enabled"`
	State              CollaborationState `json:"state"`
	Revision           uint64             `json:"revision"`
	Sequence           uint64             `json:"sequence"`
	CommandID          string             `json:"command_id"`
	ToolCallID         string             `json:"tool_call_id"`
	Reason             string             `json:"reason"`
}

type CollaborationEvent struct {
	Type        string                 `json:"-"`
	ShellID     string                 `json:"shell_id"`
	Revision    uint64                 `json:"revision"`
	Sequence    uint64                 `json:"sequence"`
	State       CollaborationState     `json:"state,omitempty"`
	Enabled     bool                   `json:"enabled,omitempty"`
	CommandID   string                 `json:"command_id,omitempty"`
	ToolCallID  string                 `json:"tool_call_id,omitempty"`
	StartOffset int64                  `json:"start_offset"`
	EndOffset   int64                  `json:"end_offset"`
	ExitCode    *int                   `json:"exit_code,omitempty"`
	ResultKind  string                 `json:"result_kind,omitempty"`
	Reason      string                 `json:"reason,omitempty"`
	Snapshot    *CollaborationSnapshot `json:"collaboration,omitempty"`
}

type outputRange struct{ start, end int64 }
type activitySegment struct {
	start int64
	data  []byte
}
type activityReservation struct {
	id, shellID string
	start, end  int64
}
type markerWaiter struct {
	nonce       string
	finalMarker byte
	ch          chan protocol.Marker
}

type Shell struct {
	id        string
	sessionID string
	cwd       string
	process   Process

	mu          sync.Mutex
	writeMu     sync.Mutex
	output      []byte
	outputStart int
	baseOffset  int64
	nextOffset  int64
	exited      bool
	exitCode    int
	lastUsed    time.Time
	changed     chan struct{}

	generationCtx      context.Context
	generationCancel   context.CancelFunc
	protocol           protocol.Parser
	markerWaiter       *markerWaiter
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
	collaborationState         CollaborationState
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
	events                     []CollaborationEvent

	commandLease chan struct{}
	leaseWaiters int

	activityCursor            int64
	shellResultActivityCursor int64
	activityFloor             int64
	activityReservation       *activityReservation
	activitySegments          []activitySegment
	activityBytes             int
	claimedRanges             []outputRange
}

type OutputSnapshot struct {
	Reset      bool
	BaseOffset int64
	NextOffset int64
	DataOffset int64
	Data       []byte
	Exited     bool
	ExitCode   int
	Changed    <-chan struct{}
}

func NewShell(id, sessionID, cwd string) *Shell {
	generationCtx, generationCancel := context.WithCancel(context.Background())
	lease := make(chan struct{}, 1)
	lease <- struct{}{}
	return &Shell{
		id: id, sessionID: sessionID, cwd: cwd, lastUsed: time.Now(), changed: make(chan struct{}),
		generationCtx: generationCtx, generationCancel: generationCancel,
		collaborationState: CollaborationOff, collaborationSupported: true, commandLease: lease,
	}
}

func (s *Shell) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// AppendOutput records PTY output, advancing replay, protocol, and activity state.
func (s *Shell) AppendOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	startOffset := s.nextOffset
	s.nextOffset += int64(len(data))
	s.lastUsed = time.Now()
	if len(data) >= ReplayBytes {
		s.output = append(s.output[:0], data[len(data)-ReplayBytes:]...)
		s.outputStart = 0
		s.baseOffset = s.nextOffset - int64(len(s.output))
	} else {
		s.output = append(s.output, data...)
		if overflow := len(s.output) - s.outputStart - ReplayBytes; overflow > 0 {
			s.outputStart += overflow
			s.baseOffset += int64(overflow)
		}
		// Reclaim dropped prefixes in replay-window-sized batches so steady PTY
		// reads do not copy the retained window on every output chunk.
		if s.outputStart >= ReplayBytes {
			activeLen := len(s.output) - s.outputStart
			copy(s.output, s.output[s.outputStart:])
			s.output = s.output[:activeLen]
			s.outputStart = 0
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

func (s *Shell) watchExit() {
	exit, ok := <-s.process.Done()
	if !ok {
		exit = Exit{Code: -1}
	}
	s.invalidate(exit.Code)
}

// Alive reports whether the shell process has not exited.
func (s *Shell) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.exited
}

func (s *Shell) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *Shell) idleSince() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsed
}

func (s *Shell) Snapshot(offset int64) OutputSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
	visibleNextOffset := s.visibleNextOffsetLocked()
	reset := offset < s.baseOffset || offset > visibleNextOffset
	if reset {
		offset = s.baseOffset
	}
	available := visibleNextOffset - offset
	if available > chunkBytes {
		available = chunkBytes
	}
	start := s.outputStart + int(offset-s.baseOffset)
	end := start + int(available)
	var data []byte
	if start >= s.outputStart && end <= len(s.output) && start < end {
		data = append([]byte(nil), s.output[start:end]...)
	}
	return OutputSnapshot{
		Reset: reset, BaseOffset: s.baseOffset, NextOffset: visibleNextOffset,
		DataOffset: offset, Data: data, Exited: s.exited, ExitCode: s.exitCode, Changed: s.changed,
	}
}

func (s *Shell) write(data []byte) error {
	return s.writeFrom(writeBrowser, data)
}

func (s *Shell) Resize(cols, rows int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.Alive() {
		return io.ErrClosedPipe
	}
	if err := s.process.Resize(cols, rows); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *Shell) invalidate(exitCode int) {
	s.writeMu.Lock()
	s.mu.Lock()
	if !s.exited {
		s.exited = true
		s.exitCode = exitCode
		s.lastUsed = time.Now()
		s.transitionCollaborationLocked(CollaborationOff, false, "collaboration", "shell exited")
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

func (s *Shell) close() {
	s.invalidate(-1)
	if s.process != nil {
		s.process.Close()
	}
}

// ValidSize reports whether cols and rows are within the supported terminal bounds.
func ValidSize(cols, rows int) bool {
	return cols >= minCols && cols <= maxCols && rows >= minRows && rows <= maxRows
}

// ID returns the shell generation identifier.
func (s *Shell) ID() string { return s.id }

// CWD returns the working directory the shell was started in.
func (s *Shell) CWD() string { return s.cwd }

// SetProcess installs the PTY process for a shell built with NewShell. Managers
// create and install processes themselves; this exists for embedding and tests.
func (s *Shell) SetProcess(process Process) { s.process = process }

// WriteBrowserInput writes browser-originated keystrokes, queueing them while an
// agent command is being injected.
func (s *Shell) WriteBrowserInput(ctx context.Context, data []byte) error {
	return s.writeFromContext(ctx, writeBrowser, data)
}

// LockCollaboration serializes collaboration enable/disable mutations.
func (s *Shell) LockCollaboration() { s.collaborationMutation.Lock() }

// UnlockCollaboration releases the lock taken by LockCollaboration.
func (s *Shell) UnlockCollaboration() { s.collaborationMutation.Unlock() }
