package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ClientToolBinding identifies one foreground owner. Generation must change on
// navigation, reconnect or ownership change; none of those renew an old lease.
type ClientToolBinding struct {
	ResponseID string
	SessionID  string
	UserID     string
	ClientID   string
	Generation string
}

// ClientToolLeaseProvider is implemented only by inline runners whose host can
// reliably revoke the originating response/client binding. A plain runner (or
// one returning nil) remains usable directly, but cannot be captured by cells.
type ClientToolLeaseProvider interface {
	ClientToolLease() *ClientToolLease
}

// ClientToolError distinguishes rejection before dispatch from uncertain device
// effects. Callers must never automatically replay outcome_unknown calls.
type ClientToolError struct {
	Code    string
	Outcome string
	Err     error
}

func (e *ClientToolError) Error() string {
	return fmt.Sprintf("%s outcome=%s: %v", e.Code, e.Outcome, e.Err)
}
func (e *ClientToolError) Unwrap() error { return e.Err }

const clientToolLeaseCallLimit = 128

// ClientToolLease is a host-owned, irreversibly revocable foreground capability.
// Constructing it asserts that the host has reliable response AND client
// lifecycle hooks. Request context cancellation alone is not such a contract.
// Revoke affects client calls only, never the surrounding cell/server tools.
type ClientToolLease struct {
	mu      sync.Mutex
	binding ClientToolBinding
	specs   map[string]ToolSpec
	ctx     context.Context
	cancel  context.CancelFunc
	reason  string
	calls   map[string]struct{} // bounded tombstones: no duplicate dispatch/replay
}

func NewClientToolLease(binding ClientToolBinding, specs []ToolSpec) (*ClientToolLease, error) {
	if binding.ResponseID == "" || binding.SessionID == "" || binding.ClientID == "" || binding.Generation == "" {
		return nil, errors.New("client lease requires response, session, client and generation identity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &ClientToolLease{binding: binding, specs: make(map[string]ToolSpec, len(specs)), ctx: ctx, cancel: cancel, calls: make(map[string]struct{})}
	for _, spec := range specs {
		if spec.Name == "" || spec.Schema == nil {
			return nil, errors.New("client lease requires complete offered tool schemas")
		}
		if _, ok := l.specs[spec.Name]; ok {
			return nil, fmt.Errorf("duplicate client tool %q", spec.Name)
		}
		copy, err := cloneClientToolSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("snapshot client tool %q: %w", spec.Name, err)
		}
		l.specs[spec.Name] = copy
	}
	return l, nil
}

func cloneClientToolSpec(spec ToolSpec) (ToolSpec, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return ToolSpec{}, err
	}
	var copy ToolSpec
	err = json.Unmarshal(raw, &copy)
	return copy, err
}

func (l *ClientToolLease) Binding() ClientToolBinding { return l.binding }
func (l *ClientToolLease) Done() <-chan struct{}      { return l.ctx.Done() }

func (l *ClientToolLease) Revoke(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reason != "" {
		return
	}
	if reason == "" {
		reason = "originating foreground binding ended"
	}
	l.reason = reason
	l.cancel()
}

func (l *ClientToolLease) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkLocked()
}

// AcceptResult serializes a small in-memory receipt update with revocation.
// A nil lease denotes an ordinary direct call. The callback must not perform
// transport I/O, acquire host locks, or call lease methods; callers acquire any
// host/mailbox locks before this guard, never from inside it.
func (l *ClientToolLease) AcceptResult(accept func()) bool {
	if l == nil {
		accept()
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reason != "" {
		return false
	}
	accept()
	return true
}

func (l *ClientToolLease) checkLocked() error {
	if l.reason != "" {
		return &ClientToolError{Code: "client_binding_expired", Outcome: "not_dispatched", Err: errors.New(l.reason)}
	}
	return nil
}

// Spec returns an independent copy; neither request nor script can mutate the
// captured schema or name catalogue after admission.
func (l *ClientToolLease) Spec(name string) (ToolSpec, bool) {
	spec, ok := l.specs[name]
	if !ok {
		return ToolSpec{}, false
	}
	copy, err := cloneClientToolSpec(spec)
	return copy, err == nil
}

type clientToolDispatchKey struct{}
type clientToolDispatch struct {
	lease   *ClientToolLease
	started bool // guarded by lease.mu
}

// ClientToolCallLease identifies a lease-admitted call for transport fencing.
// It is nil for ordinary direct inline calls, whose recovery stays unchanged.
func ClientToolCallLease(ctx context.Context) *ClientToolLease {
	if dispatch, _ := ctx.Value(clientToolDispatchKey{}).(*clientToolDispatch); dispatch != nil {
		return dispatch.lease
	}
	return nil
}

// ClientToolEventDeliverable fences marked foreground requests at FINAL stream
// delivery, not just initial publication: stored frames must not revive device
// actions on a reconnect after their originating foreground has expired.
func ClientToolEventDeliverable(ctx context.Context, raw json.RawMessage, lease *ClientToolLease) bool {
	var marker struct {
		Foreground bool `json:"foreground_only"`
	}
	if json.Unmarshal(raw, &marker) != nil {
		return false
	}
	if !marker.Foreground {
		return true
	}
	owner, _ := ctx.Value(clientToolForegroundContextKey{}).(*ClientToolForeground)
	return owner != nil && lease != nil && owner.generation == lease.Binding().Generation && lease.Check() == nil
}

// BeginClientToolDispatch is called by a trusted inline transport immediately
// before publishing to the originating device, AFTER its own queue/gate waits.
// It atomically fences publication against lease revocation. Ordinary direct
// calls have no lease marker and retain their existing cancellation semantics.
func BeginClientToolDispatch(ctx context.Context) error {
	d, _ := ctx.Value(clientToolDispatchKey{}).(*clientToolDispatch)
	if d == nil {
		return ctx.Err()
	}
	d.lease.mu.Lock()
	defer d.lease.mu.Unlock()
	if err := d.lease.checkLocked(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return &ClientToolError{Code: "cancelled", Outcome: "not_dispatched", Err: err}
	}
	d.started = true
	return nil
}

// Dispatch reserves a bounded call ID; the inline runner must call
// BeginClientToolDispatch at its actual publication boundary. Cancellation
// before that boundary is not_dispatched; after it, device effects are unknown.
// Outstanding runner goroutines and tombstones are bounded by the call budget.
func (l *ClientToolLease) Dispatch(ctx context.Context, runner ClientToolRunner, call ToolCall) (ToolOutput, error) {
	l.mu.Lock()
	err := l.admitLocked(ctx, runner, call)
	l.mu.Unlock()
	if err != nil {
		return ToolOutput{}, err
	}
	call.Arguments = append(json.RawMessage(nil), call.Arguments...)
	dispatch := &clientToolDispatch{lease: l}
	callCtx, cancel := context.WithCancel(context.WithValue(ctx, clientToolDispatchKey{}, dispatch))
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	type result struct {
		out ToolOutput
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			if recovered := recover(); recovered != nil {
				r.err = fmt.Errorf("client runner panic: %v", recovered)
			}
			resultCh <- r
		}()
		r.out, r.err = runner.RunClientTool(callCtx, call)
	}()
	select {
	case r := <-resultCh:
		// Serialize acceptance with revocation, not with a racy select order.
		l.mu.Lock()
		expired := l.reason != ""
		l.mu.Unlock()
		if expired {
			return ToolOutput{}, dispatch.failure("client_binding_expired", errors.New("foreground binding ended during client call"))
		}
		if ctx.Err() != nil {
			return ToolOutput{}, dispatch.failure("cancelled", ctx.Err())
		}
		if r.err != nil {
			return ToolOutput{}, dispatch.failure("client_tool_error", r.err)
		}
		return r.out, nil
	case <-l.Done():
		return ToolOutput{}, dispatch.failure("client_binding_expired", errors.New("foreground binding ended during client call"))
	case <-ctx.Done():
		return ToolOutput{}, dispatch.failure("cancelled", ctx.Err())
	}
}

func (l *ClientToolLease) admitLocked(ctx context.Context, runner ClientToolRunner, call ToolCall) error {
	if err := l.checkLocked(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return &ClientToolError{Code: "cancelled", Outcome: "not_dispatched", Err: err}
	}
	if _, ok := l.specs[call.Name]; !ok || runner == nil || !runner.OwnsClientTool(call.Name) {
		return &ClientToolError{Code: "client_tool_unoffered", Outcome: "not_dispatched", Err: fmt.Errorf("tool %q was not offered", call.Name)}
	}
	if _, exists := l.calls[call.ID]; exists || call.ID == "" {
		return &ClientToolError{Code: "client_call_duplicate", Outcome: "not_dispatched", Err: errors.New("client call ID missing or already dispatched")}
	}
	if len(l.calls) >= clientToolLeaseCallLimit {
		return &ClientToolError{Code: "call_limit", Outcome: "not_dispatched", Err: errors.New("client lease call budget exhausted")}
	}
	l.calls[call.ID] = struct{}{}
	return nil
}

func (d *clientToolDispatch) failure(code string, err error) error {
	d.lease.mu.Lock()
	defer d.lease.mu.Unlock()
	outcome := "not_dispatched"
	if d.started {
		outcome = "outcome_unknown"
	}
	return &ClientToolError{Code: code, Outcome: outcome, Err: err}
}

// ClientToolForeground owns the ORIGINAL host transport connection, not a model
// run's context or a reconnectable observation. The host must close it when that
// foreground stream returns; its context must signal original-client disconnect.
// One foreground can bind once only, so recovery cannot create a renewed lease.
type ClientToolForeground struct {
	mu         sync.Mutex
	ctx        context.Context
	generation string
	closed     bool
	bound      bool
	lease      *ClientToolLease
	stop       func() bool
}

type clientToolForegroundContextKey struct{}

// Context marks the original host stream. Recovery requests cannot supply this
// opaque value, even while the originating response and lease remain active.
func (f *ClientToolForeground) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, clientToolForegroundContextKey{}, f)
}

func NewClientToolForeground(ctx context.Context, generation string) *ClientToolForeground {
	f := &ClientToolForeground{ctx: ctx, generation: generation}
	f.stop = context.AfterFunc(ctx, f.revoke)
	return f
}

func (f *ClientToolForeground) Bind(responseID, sessionID string, specs []ToolSpec) *ClientToolLease {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.bound || f.ctx.Err() != nil {
		return nil
	}
	f.bound = true
	lease, err := NewClientToolLease(ClientToolBinding{
		ResponseID: responseID, SessionID: sessionID,
		ClientID: f.generation, Generation: f.generation,
	}, specs)
	if err == nil {
		f.lease = lease
	}
	return f.lease
}

func (f *ClientToolForeground) revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.lease != nil {
		f.lease.Revoke("originating foreground transport ended")
	}
}

func (f *ClientToolForeground) Close() {
	f.stop()
	f.revoke()
}
