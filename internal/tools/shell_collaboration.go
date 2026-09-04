package tools

import (
	"context"
	"errors"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ShellRoutingMode determines whether a shell tool may use the local executor.
type ShellRoutingMode string

const (
	ShellRoutingLocalOnly          ShellRoutingMode = "local_only"
	ShellRoutingControllerRequired ShellRoutingMode = "controller_required"
)

// CollaborativeShellState is the controller's authoritative shared-shell state.
type CollaborativeShellState string

const (
	CollaborativeShellOff            CollaborativeShellState = "off"
	CollaborativeShellReady          CollaborativeShellState = "ready"
	CollaborativeShellAgentRunning   CollaborativeShellState = "agent_running"
	CollaborativeShellDesynchronized CollaborativeShellState = "desynchronized"
	CollaborativeShellUnavailable    CollaborativeShellState = "unavailable"
)

// CollaborativeShellMode is a point-in-time routing snapshot. Enabled remains
// true for attention states so a run cannot silently fall back to local execution.
type CollaborativeShellMode struct {
	State                CollaborativeShellState
	ShellID              string
	Enabled              bool
	Reason               string
	ActivityOffset       int64
	BrowserInputRevision uint64
}

// CollaborativeShellActivityFence records the newest terminal offset visible to
// one model run. The controller advances it when a shell call is rejected with
// fresh browser activity, allowing the model to reconsider and retry safely.
type CollaborativeShellActivityFence struct {
	mu                   sync.Mutex
	offset               int64
	browserInputRevision uint64
}

func NewCollaborativeShellActivityFence(offset int64, browserInputRevision ...uint64) *CollaborativeShellActivityFence {
	fence := &CollaborativeShellActivityFence{offset: offset}
	if len(browserInputRevision) > 0 {
		fence.browserInputRevision = browserInputRevision[0]
	}
	return fence
}

func (f *CollaborativeShellActivityFence) Snapshot() (int64, uint64) {
	if f == nil {
		return 0, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offset, f.browserInputRevision
}

func (f *CollaborativeShellActivityFence) Offset() int64 {
	offset, _ := f.Snapshot()
	return offset
}

func (f *CollaborativeShellActivityFence) Advance(offset int64, browserInputRevision ...uint64) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if offset > f.offset {
		f.offset = offset
	}
	if len(browserInputRevision) > 0 && browserInputRevision[0] > f.browserInputRevision {
		f.browserInputRevision = browserInputRevision[0]
	}
	f.mu.Unlock()
}

// CollaborativeShellRunBinding pins authority for one complete engine run.
type CollaborativeShellRunBinding struct {
	Required bool
	ShellID  string
	Fence    *CollaborativeShellActivityFence
}

type collaborativeShellBindingKey struct{}

func ContextWithCollaborativeShellRunBinding(ctx context.Context, binding CollaborativeShellRunBinding) context.Context {
	return context.WithValue(ctx, collaborativeShellBindingKey{}, binding)
}

func CollaborativeShellRunBindingFromContext(ctx context.Context) (CollaborativeShellRunBinding, bool) {
	binding, ok := ctx.Value(collaborativeShellBindingKey{}).(CollaborativeShellRunBinding)
	return binding, ok
}

// SharedShellArgs contains internal execution metadata that is never model-visible.
type SharedShellArgs struct {
	Command         string
	TimeoutSeconds  int
	ToolCallID      string
	OutputLimit     int64
	ExpectedShellID string
	ActivityFence   *CollaborativeShellActivityFence
}

// SharedShellActivity is a reserved, generation-bound terminal activity range.
type SharedShellActivity struct {
	ID                   string
	ShellID              string
	StartOffset          int64
	EndOffset            int64
	BrowserInputRevision uint64
	Excerpt              string
	Truncated            bool
}

// CollaborativeShellController is the transport-neutral bridge used by ShellTool.
type CollaborativeShellController interface {
	Mode(ctx context.Context, sessionID string) CollaborativeShellMode
	Execute(ctx context.Context, sessionID string, args SharedShellArgs) (ShellResult, error)
	PrepareRequestContext(ctx context.Context, sessionID string, messages []llm.Message) ([]llm.Message, error)
	PrepareCompactionContext(ctx context.Context, sessionID string, result *llm.CompactionResult) error
}

// CollaborativeShellActivityController advances terminal activity only after
// its matching transcript boundary has committed.
type CollaborativeShellActivityController interface {
	ReserveActivity(ctx context.Context, sessionID, expectedShellID string) (*SharedShellActivity, error)
	CommitDurableActivity(ctx context.Context, sessionID string, activity SharedShellActivity) error
	CommitActivity(ctx context.Context, sessionID, reservationID string) error
	ReleaseActivity(ctx context.Context, sessionID, reservationID string)
}

// CollaborativeShellError has a stable machine-readable failure kind.
type CollaborativeShellError struct {
	Kind    string
	Message string
}

func (e *CollaborativeShellError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func NewCollaborativeShellError(kind, message string) error {
	return &CollaborativeShellError{Kind: kind, Message: message}
}

func CollaborativeShellErrorKind(err error) string {
	var target *CollaborativeShellError
	if errors.As(err, &target) {
		return target.Kind
	}
	return ""
}
