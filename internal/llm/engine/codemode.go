package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/term-llm/term-llm/internal/codemode"
	"github.com/term-llm/term-llm/internal/llm/core"
	corestream "github.com/term-llm/term-llm/internal/llm/core/stream"
)

var nestedProcessPermits = make(chan struct{}, 16)

type nestedContextKey struct{}
type nestedLease struct {
	permitMu   sync.Mutex
	held       bool
	pause      func() (func() error, error)
	once       sync.Once
	started    bool
	background bool
	parent     string
	finish     func()
}

func (l *nestedLease) done() { l.once.Do(l.finish) }
func nestedExecution(ctx context.Context) bool {
	lease, _ := ctx.Value(nestedContextKey{}).(*nestedLease)
	return lease != nil
}

type nestedIdentity struct {
	spec       core.ToolSpec
	hash       string
	generation uint64
}
type codeModule struct {
	generation string
	source     core.CodeModeModules
	requires   []string
	trusted    func() bool
}
type nestedCell struct {
	engine         *Engine
	parent         string
	send           corestream.Sender
	eventMu        sync.RWMutex
	eventSend      corestream.Sender
	background     bool
	ceiling        map[string]nestedIdentity
	modules        map[string]codeModule
	permits        chan struct{}
	mu             sync.Mutex
	closed         bool
	live           int
	waiting        int
	awaiting       int
	outcomes       []core.CodeModeCallOutcome
	images         map[string]core.ToolImageData
	imageBytes     int
	doneOnce       sync.Once
	done           chan struct{}
	sessionPermits chan struct{}
	inflight       chan struct{}
	authority      core.CodeModeAuthority
	clients        *codeModeClientTools
	serverTools    map[string]core.Tool
}

// SetCodeModeDirectOnly sets an additional narrowing-only exclusion list.
// Deprecated: declare CodeModeExposure on the registered tool instead. Removing
// an exclusion never grants exposure to an undeclared or direct-only instance.
func (e *Engine) SetCodeModeDirectOnly(names []string) {
	e.codeModeMu.Lock()
	defer e.codeModeMu.Unlock()
	e.codeModeDirectOnly = make(map[string]bool, len(names))
	for _, n := range names {
		e.codeModeDirectOnly[n] = true
	}
}

func (e *Engine) admitCodeCell(ctx context.Context, parent string, send corestream.Sender) (*nestedCell, error) {
	// The explicit synchronous compatibility lane remains mutually exclusive
	// with async cells, so it cannot add hidden session tool/memory capacity.
	if s := e.codeModeSessions; s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return nil, &codemode.Error{Code: "engine_closed", Message: "engine closed"}
		}
		for _, o := range s.sessions {
			if len(o.live) > 0 {
				return nil, &codemode.Error{Code: "busy", Message: "async cells or unresolved effects still active"}
			}
		}
	}
	cell, err := e.admitCodeCellFor(ctx, parent, send, codemode.LegacyToolName, false)
	if err != nil {
		return nil, err
	}
	if err := e.captureCodeModeAuthority(ctx, core.CodeModeCellRef{SessionID: core.SessionIDFromContext(ctx), CellID: parent, Ctx: ctx}, cell); err != nil {
		cell.Close()
		return nil, err
	}
	return cell, nil
}

// admitCodeCellFor snapshots one immutable capability ceiling. Background
// cells are async: their child events are best-effort so a retired request
// stream can never block an external call.
func (e *Engine) admitCodeCellFor(ctx context.Context, parent string, send corestream.Sender, required string, background bool) (*nestedCell, error) {
	e.codeModeMu.Lock()
	defer e.codeModeMu.Unlock()
	if e.codeModeCell != nil {
		return nil, &codemode.Error{Code: "busy", Message: "cell or unresolved child still active"}
	}
	if !e.IsToolAllowed(required) {
		return nil, &codemode.Error{Code: "permission_denied", Message: required + " not allowed"}
	}
	c := &nestedCell{engine: e, parent: parent, send: send, background: background, ceiling: map[string]nestedIdentity{}, modules: map[string]codeModule{}, permits: make(chan struct{}, codemode.Limits.Children), images: map[string]core.ToolImageData{}, done: make(chan struct{})}
	if background {
		c.eventSend = send
		c.send = corestream.NewRoutedSender(c.currentSender)
	}
	registered, generations := e.tools.codeModeExecutionSnapshot()
	clients, err := captureCodeModeClients(ctx, registered)
	if err != nil {
		return nil, err
	}
	c.clients = clients
	c.serverTools = make(map[string]core.Tool)
	for name, identity := range clients.identities() {
		c.ceiling[name] = identity
	}
	for name, tool := range registered {
		if !e.codeModeExposureLocked(name, tool).Allowed {
			continue
		}
		spec, hash, err := codeIdentity(ctx, tool)
		if err == nil {
			c.ceiling[name] = nestedIdentity{spec: spec, hash: hash, generation: generations[name]}
			c.serverTools[name] = tool
		}
	}
	for n, m := range e.codeModules {
		c.modules[n] = m
	}
	if !background {
		e.codeModeCell = c
	}
	return c, nil
}

// codeModeExposure is the single effective server-tool exposure policy. It is
// deliberately instance-based: a remote tool with a builtin-looking name cannot
// inherit local privileges, and replacement can narrow an existing cell.
func (e *Engine) codeModeExposure(name string, tool core.Tool) core.CodeModeExposure {
	e.codeModeMu.Lock()
	defer e.codeModeMu.Unlock()
	return e.codeModeExposureLocked(name, tool)
}

// codeModeExposureLocked requires codeModeMu, including during cell admission.
func (e *Engine) codeModeExposureLocked(name string, tool core.Tool) core.CodeModeExposure {
	if tool == nil {
		return core.CodeModeExposure{Reason: "tool_removed"}
	}
	if f, ok := tool.(core.FinishingTool); ok && f.IsFinishingTool() {
		return core.CodeModeExposure{Reason: "finishing_tool"}
	}
	// Lifecycle tools cannot compose themselves, even if a host supplies metadata.
	// Check instance semantics, not names: MCP names are arbitrary.
	if control, ok := tool.(core.CodeModeLifecycleTool); ok && control.IsCodeModeLifecycleTool() {
		return core.CodeModeExposure{Reason: "script_lifecycle"}
	}
	if e.codeModeDirectOnly[name] {
		return core.CodeModeExposure{Reason: "host_direct_only"}
	}
	if !e.IsToolAllowed(name) {
		return core.CodeModeExposure{Reason: "permission_denied"}
	}
	if p, ok := tool.(core.CodeModeExposureProvider); ok {
		exposure := p.CodeModeExposure()
		if exposure.Allowed {
			return core.CodeModeExposure{Allowed: true}
		}
		if exposure.Reason != "" {
			return exposure
		}
	}
	return core.CodeModeExposure{Reason: "undeclared_exposure"}
}

func codeIdentity(ctx context.Context, t core.Tool) (core.ToolSpec, string, error) {
	var spec core.ToolSpec
	var hash string
	if live, ok := t.(interface {
		CodeModeIdentity(context.Context) (core.ToolSpec, string, error)
	}); ok {
		var err error
		spec, hash, err = live.CodeModeIdentity(ctx)
		if err != nil {
			return spec, "", err
		}
	} else {
		spec = t.Spec()
		raw, err := json.Marshal(spec)
		if err != nil {
			return spec, "", err
		}
		hash = fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	// A remote target/credential change can happen before catalogue refresh
	// replaces its registry wrapper. Include live binding identity as well as
	// the schema so that gap cannot retarget an admitted capability.
	if binding := core.ToolBindingIDOf(t); binding != "" {
		hash = fmt.Sprintf("%x", sha256.Sum256([]byte(hash+"\x00"+binding)))
	}
	return spec, hash, nil
}
func (c *nestedCell) Names() []string {
	names := make([]string, 0, len(c.ceiling))
	for n := range c.ceiling {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
func (c *nestedCell) identity(ctx context.Context, name string) (nestedIdentity, error) {
	frozen, ok := c.ceiling[name]
	if !ok {
		return nestedIdentity{}, &codemode.Error{Code: "permission_denied", Message: "tool outside admitted capability ceiling: " + name}
	}
	if identity, handled, err := c.clients.identity(name, c.engine.tools.executionSnapshot()); handled {
		return identity, err
	}
	if !c.engine.IsToolAllowed(name) {
		return nestedIdentity{}, &codemode.Error{Code: "permission_denied", Message: "tool outside admitted capability ceiling: " + name}
	}
	tool, generation, ok := c.engine.tools.codeModeExecutionBinding(name)
	if !ok || !c.engine.codeModeExposure(name, tool).Allowed {
		return nestedIdentity{}, &codemode.Error{Code: "permission_denied", Message: "tool removed or direct-only: " + name}
	}
	spec, hash, err := codeIdentity(ctx, tool)
	if err != nil {
		return nestedIdentity{}, &codemode.Error{Code: "permission_denied", Message: err.Error()}
	}
	if hash != frozen.hash || spec.Name != frozen.spec.Name || generation != frozen.generation {
		return nestedIdentity{}, &codemode.Error{Code: "schema_changed", Message: "tool schema or binding changed; rediscover before a new cell: " + name}
	}
	return nestedIdentity{spec: spec, hash: hash, generation: generation}, nil
}
func (c *nestedCell) Describe(ctx context.Context, names []string) (json.RawMessage, error) {
	if len(names) > codemode.Limits.Calls {
		return nil, &codemode.Error{Code: "description_limit", Message: "too many names"}
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		id, err := c.identity(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": name, "description": truncateUTF8(id.spec.Description, 1024), "inputSchema": id.spec.Schema, "outputSchema": id.spec.OutputSchema, "schema_hash": id.hash, "returns": "MCP: {content, structuredContent?, isError?}; native: {text,isError,timedOut,content?}; image blocks use cell-scoped handle, explicit image(block) emits"})
	}
	b, err := json.Marshal(out)
	if len(b) > codemode.Limits.Arguments {
		return nil, &codemode.Error{Code: "description_limit", Message: "schemas too large; request fewer"}
	}
	return b, err
}
func (c *nestedCell) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if c.inflight != nil {
		select {
		case c.inflight <- struct{}{}:
			defer func() { <-c.inflight }()
		default:
			return nil, &codemode.Error{Code: "call_limit", Message: "session in-flight call limit", Outcome: "not_dispatched"}
		}
	}
	call := core.ToolCall{ID: core.NewSyntheticToolCallID(), Name: name, Arguments: append(json.RawMessage(nil), args...)}
	if len(args) > codemode.Limits.Arguments || !json.Valid(args) {
		return nil, c.failure(call, "arguments_limit", "not_dispatched", "invalid or oversized arguments")
	}
	if _, err := c.identity(ctx, name); err != nil {
		return nil, c.failure(call, errorCode(err), "not_dispatched", err.Error())
	}
	lease, err := c.acquire(ctx)
	if err != nil {
		return nil, c.failure(call, "cancelled", "not_dispatched", err.Error())
	}
	defer func() {
		if !lease.started {
			lease.done()
		}
	}()
	// Recheck after admission wait: activation/removal/revocation cannot widen authority.
	if _, err = c.identity(ctx, name); err != nil {
		return nil, c.failure(call, errorCode(err), "not_dispatched", err.Error())
	}
	toolCtx := context.WithValue(ctx, nestedContextKey{}, lease)
	if c.background {
		toolCtx = core.ContextWithCodeModeToolPause(toolCtx, lease.pause)
	}
	// Pin the admitted execution instance even if a registry replacement races
	// the final identity check. Scoped authority overrides refine this instance.
	if tool, ok := c.serverTools[name]; ok {
		toolCtx = context.WithValue(toolCtx, nestedToolOverrideKey{}, tool)
	}
	if c.authority != nil {
		if tool, ok := c.authority.Tool(name); ok {
			toolCtx = context.WithValue(toolCtx, nestedToolOverrideKey{}, tool)
		}
	}
	if tool, ok := c.clients.tool(name); ok {
		toolCtx = context.WithValue(toolCtx, nestedToolOverrideKey{}, tool)
	}
	startEvent := core.Event{Type: core.EventToolExecStart, ParentToolCallID: c.parent, ToolCallID: call.ID, ToolName: name, ToolArgs: call.Arguments}
	if c.background {
		c.send.TrySend(startEvent)
	} else {
		_ = c.send.Send(startEvent)
	}
	audit := core.CodeModeCallOutcome{ID: call.ID, Name: name, Status: "executing", Outcome: "outcome_unknown"}
	if len(args) <= 4096 {
		audit.Arguments = append(json.RawMessage(nil), args...)
	} else {
		audit.ArgumentsOmitted = true
	}
	c.record(audit)
	outcome := c.engine.executeSingleToolCallOutcomeSafe(toolCtx, call, c.send, false, false)
	c.record(core.CodeModeCallOutcome{ID: call.ID, Name: name, Status: "executing", Outcome: "outcome_unknown", GuardianReviews: outcome.output.GuardianReviews})
	if outcome.err != nil {
		var clientErr *core.ClientToolError
		if errors.As(outcome.err, &clientErr) {
			return nil, c.failure(call, clientErr.Code, clientErr.Outcome, clientErr.Error())
		}
		code := "tool_error"
		if errors.Is(outcome.err, context.Canceled) || errors.Is(outcome.err, context.DeadlineExceeded) {
			code = "cancelled"
		}
		return nil, c.failure(call, code, "outcome_unknown", outcome.err.Error())
	}
	raw, err := c.project(outcome.output)
	if err != nil {
		return nil, c.failure(call, "result_limit", "completed", err.Error())
	}
	c.record(core.CodeModeCallOutcome{ID: call.ID, Name: name, Status: "completed", Outcome: "completed", IsError: outcome.output.IsError})
	return raw, nil
}
func (c *nestedCell) failure(call core.ToolCall, code, outcome, message string) error {
	c.record(core.CodeModeCallOutcome{ID: call.ID, Name: call.Name, Status: "failed", Outcome: outcome, Error: code})
	return &codemode.Error{Code: code, Message: fmt.Sprintf("call_id=%s outcome=%s: %s", call.ID, outcome, message), CallID: call.ID, Outcome: outcome}
}
func errorCode(err error) string {
	if e, ok := err.(*codemode.Error); ok {
		return e.Code
	}
	return "host_error"
}
func (c *nestedCell) record(o core.CodeModeCallOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, previous := range c.outcomes {
		if previous.ID == o.ID {
			if len(o.Arguments) == 0 {
				o.Arguments = previous.Arguments
				o.ArgumentsOmitted = previous.ArgumentsOmitted
			}
			if len(o.GuardianReviews) == 0 {
				o.GuardianReviews = previous.GuardianReviews
			}
			c.outcomes[i] = o
			return
		}
	}
	if len(c.outcomes) < codemode.Limits.Calls {
		c.outcomes = append(c.outcomes, o)
	}
}
func (c *nestedCell) Outcomes() []core.CodeModeCallOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]core.CodeModeCallOutcome(nil), c.outcomes...)
}
func (c *nestedCell) acquire(ctx context.Context) (*nestedLease, error) {
	c.mu.Lock()
	c.waiting++
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.waiting--; c.mu.Unlock() }()
	select {
	case c.permits <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if c.sessionPermits != nil {
		select {
		case c.sessionPermits <- struct{}{}:
		case <-ctx.Done():
			<-c.permits
			return nil, ctx.Err()
		}
	}
	select {
	case nestedProcessPermits <- struct{}{}:
	case <-ctx.Done():
		<-c.permits
		if c.sessionPermits != nil {
			<-c.sessionPermits
		}
		return nil, ctx.Err()
	}
	c.engine.codeModeMu.Lock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.engine.codeModeMu.Unlock()
		<-c.permits
		<-nestedProcessPermits
		if c.sessionPermits != nil {
			<-c.sessionPermits
		}
		return nil, fmt.Errorf("cell closed")
	}
	c.live++
	c.mu.Unlock()
	c.engine.codeModeMu.Unlock()
	lease := &nestedLease{parent: c.parent, background: c.background, held: true}
	lease.finish = func() {
		<-c.permits
		lease.permitMu.Lock()
		if lease.held {
			<-nestedProcessPermits
			if c.sessionPermits != nil {
				<-c.sessionPermits
			}
			lease.held = false
		}
		lease.permitMu.Unlock()
		c.engine.codeModeMu.Lock()
		defer c.engine.codeModeMu.Unlock()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.live--
		c.finishLocked()
	}
	lease.pause = func() (func() error, error) {
		lease.permitMu.Lock()
		if !lease.held {
			lease.permitMu.Unlock()
			return nil, fmt.Errorf("tool permits already parked")
		}
		<-nestedProcessPermits
		if c.sessionPermits != nil {
			<-c.sessionPermits
		}
		lease.held = false
		lease.permitMu.Unlock()
		c.mu.Lock()
		c.awaiting++
		c.mu.Unlock()
		var once sync.Once
		var resumeErr error
		return func() error {
			once.Do(func() {
				c.mu.Lock()
				c.awaiting--
				c.mu.Unlock()
				if c.sessionPermits != nil {
					select {
					case c.sessionPermits <- struct{}{}:
					case <-ctx.Done():
						resumeErr = ctx.Err()
						return
					}
				}
				select {
				case nestedProcessPermits <- struct{}{}:
				case <-ctx.Done():
					if c.sessionPermits != nil {
						<-c.sessionPermits
					}
					resumeErr = ctx.Err()
					return
				}
				lease.permitMu.Lock()
				lease.held = true
				lease.permitMu.Unlock()
			})
			return resumeErr
		}, nil
	}
	return lease, nil
}
func (c *nestedCell) Close() {
	c.engine.codeModeMu.Lock()
	defer c.engine.codeModeMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.images = nil
	c.finishLocked()
}

// finishLocked releases the legacy slot and signals capability settlement only
// after every actual host call ends. Async cells keep their own admission place
// until this signal, even after their VM exits.
func (c *nestedCell) finishLocked() {
	if c.closed && c.live == 0 {
		c.doneOnce.Do(func() {
			if c.authority != nil {
				c.authority.Close()
			}
			close(c.done)
		})
		if c.engine.codeModeCell == c {
			c.engine.codeModeCell = nil
		}
	}
}

// Done closes when the capability is closed and no actual host call remains.
func (c *nestedCell) Done() <-chan struct{} { return c.done }

// AdmitCodeModule installs bytes already snapshotted by the trusted activation adapter.
// Dependencies are checked, never enabled or registered by this method.
func (e *Engine) AdmitCodeModule(name string, source core.CodeModeModules, requires []string, trusted func() bool) error {
	if trusted == nil || !trusted() {
		return fmt.Errorf("skill %s: code modules not trusted", name)
	}
	for _, n := range requires {
		t, ok := e.tools.Get(n)
		if !ok || !e.codeModeExposure(n, t).Allowed {
			return fmt.Errorf("skill %s requires unavailable tool %s", name, n)
		}
	}
	e.codeModeMu.Lock()
	defer e.codeModeMu.Unlock()
	if e.codeModules == nil {
		e.codeModules = map[string]codeModule{}
	}
	e.codeModules[name] = codeModule{generation: core.NewSyntheticToolCallID(), source: source, requires: append([]string(nil), requires...), trusted: trusted}
	return nil
}

// ClearCodeModules requires reactivation; transcript text is never an executable grant.
func (e *Engine) ClearCodeModules() {
	e.codeModeMu.Lock()
	defer e.codeModeMu.Unlock()
	e.codeModules = nil
}
func (c *nestedCell) module(name string) (codeModule, error) {
	n := strings.TrimPrefix(name, "skill:")
	n = strings.SplitN(n, "/", 2)[0]
	m, ok := c.modules[n]
	c.engine.codeModeMu.Lock()
	current := c.engine.codeModules[n]
	c.engine.codeModeMu.Unlock()
	if m.generation != current.generation {
		return codeModule{}, fmt.Errorf("module admission revoked: %s", name)
	}
	if !ok || m.trusted == nil || !m.trusted() {
		return codeModule{}, fmt.Errorf("module not admitted or trust revoked: %s", name)
	}
	for _, tool := range m.requires {
		if _, err := c.identity(context.Background(), tool); err != nil {
			return codeModule{}, fmt.Errorf("module dependency: %w", err)
		}
	}
	return m, nil
}
func (c *nestedCell) Normalize(base, spec string) (string, error) {
	if strings.HasPrefix(spec, "skill:") {
		owner := strings.TrimPrefix(strings.SplitN(base, "/", 2)[0], "skill:")
		if _, moduleOwned := c.modules[owner]; moduleOwned && owner != strings.TrimPrefix(spec, "skill:") {
			return "", fmt.Errorf("cross-skill module dependencies are not supported")
		}
	}

	name := base
	if strings.HasPrefix(spec, "skill:") {
		name = spec
	}
	m, err := c.module(name)
	if err != nil {
		return "", err
	}
	return m.source.Normalize(base, spec)
}
func (c *nestedCell) Source(name string) (string, error) {
	m, err := c.module(name)
	if err != nil {
		return "", err
	}
	return m.source.Source(name)
}

func nestedParent(ctx context.Context) string {
	l, _ := ctx.Value(nestedContextKey{}).(*nestedLease)
	if l != nil {
		return l.parent
	}
	return ""
}

// nestedBestEffort reports whether a background cell's child events must not
// block on a request stream that may already be retired or unconsumed.
func nestedBestEffort(ctx context.Context) bool {
	l, _ := ctx.Value(nestedContextKey{}).(*nestedLease)
	return l != nil && l.background
}

// ScopeCodeModules snapshots admission, not guest state. Restoring revokes all
// turn-local module generations, even if a cell still holds a source snapshot.
func (e *Engine) ScopeCodeModules() func() {
	e.codeModeMu.Lock()
	prior := make(map[string]codeModule, len(e.codeModules))
	for n, m := range e.codeModules {
		prior[n] = m
	}
	e.codeModeMu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { e.codeModeMu.Lock(); defer e.codeModeMu.Unlock(); e.codeModules = prior }) }
}

// Rebind only event delivery, never the admitted authority or tool context.
func (c *nestedCell) bindSender(send corestream.Sender) {
	c.eventMu.Lock()
	c.eventSend = send
	c.eventMu.Unlock()
}
func (c *nestedCell) currentSender() corestream.Sender {
	c.eventMu.RLock()
	defer c.eventMu.RUnlock()
	return c.eventSend
}
