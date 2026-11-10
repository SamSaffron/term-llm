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

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
)

var nestedProcessPermits = make(chan struct{}, 16)

type nestedContextKey struct{}
type nestedLease struct {
	once       sync.Once
	started    bool
	background bool
	parent     string
	finish     func()
}

func (l *nestedLease) done()                   { l.once.Do(l.finish) }
func nestedExecution(ctx context.Context) bool { return ctx.Value(nestedContextKey{}) != nil }

type nestedIdentity struct {
	spec core.ToolSpec
	hash string
}
type codeModule struct {
	generation string
	source     core.CodeModeModules
	requires   []string
	trusted    func() bool
}
type nestedCell struct {
	engine     *Engine
	parent     string
	send       corestream.Sender
	eventMu    sync.RWMutex
	eventSend  corestream.Sender
	background bool
	ceiling    map[string]nestedIdentity
	modules    map[string]codeModule
	permits    chan struct{}
	mu         sync.Mutex
	closed     bool
	live       int
	outcomes   []core.CodeModeCallOutcome
	images     map[string]core.ToolImageData
	imageBytes int
	doneOnce   sync.Once
	done       chan struct{}
}

// SetCodeModeDirectOnly classifies built-in authority/lifecycle tools using host metadata.
func (e *Engine) SetCodeModeDirectOnly(names []string) {
	e.codeModeMu.Lock()
	defer e.codeModeMu.Unlock()
	e.codeModeDirectOnly = make(map[string]bool, len(names))
	for _, n := range names {
		e.codeModeDirectOnly[n] = true
	}
}

func (e *Engine) admitCodeCell(ctx context.Context, parent string, send corestream.Sender) (*nestedCell, error) {
	return e.admitCodeCellFor(ctx, parent, send, codemode.LegacyToolName, false)
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
	for name, tool := range e.tools.executionSnapshot() {
		if !e.IsToolAllowed(name) || e.codeModeDirectOnly[name] || directOnly(name, tool) {
			continue
		}
		spec, hash, err := codeIdentity(ctx, tool)
		if err == nil {
			c.ceiling[name] = nestedIdentity{spec, hash}
		}
	}
	for n, m := range e.codeModules {
		c.modules[n] = m
	}
	e.codeModeCell = c
	return c, nil
}

func directOnly(name string, tool core.Tool) bool {
	if f, ok := tool.(core.FinishingTool); ok && f.IsFinishingTool() {
		return true
	}
	switch name {
	case codemode.LegacyToolName, codemode.ExecToolName, codemode.WaitToolName, codemode.CancelToolName, "activate_skill", "search_skills", "tool_search", "suggest_commands", "ask_user", "spawn_agent", "wait_agent", "continue_agent", "cancel_agent", "list_agents", "queue_agent", "wait_for_jobs", "initiate_handover", "hub_delegate", "hub_check_delegation", "manage_workspace", "update_plan", "live_settings", "live_new_session", "live_switch_session", "ui_extensions", "ui_activate", "create_goal", "update_goal", "update_progress", "finalize_progress", "run_agent_script":
		return true
	}
	return false
}
func codeIdentity(ctx context.Context, t core.Tool) (core.ToolSpec, string, error) {
	if live, ok := t.(interface {
		CodeModeIdentity(context.Context) (core.ToolSpec, string, error)
	}); ok {
		return live.CodeModeIdentity(ctx)
	}
	spec := t.Spec()
	raw, err := json.Marshal(spec)
	if err != nil {
		return spec, "", err
	}
	return spec, fmt.Sprintf("%x", sha256.Sum256(raw)), nil
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
	if !ok || !c.engine.IsToolAllowed(name) {
		return nestedIdentity{}, &codemode.Error{Code: "permission_denied", Message: "tool outside admitted capability ceiling: " + name}
	}
	tool, ok := c.engine.tools.Get(name)
	if !ok || directOnly(name, tool) {
		return nestedIdentity{}, &codemode.Error{Code: "permission_denied", Message: "tool removed or direct-only: " + name}
	}
	spec, hash, err := codeIdentity(ctx, tool)
	if err != nil {
		return nestedIdentity{}, &codemode.Error{Code: "permission_denied", Message: err.Error()}
	}
	if hash != frozen.hash || spec.Name != frozen.spec.Name {
		return nestedIdentity{}, &codemode.Error{Code: "schema_changed", Message: "rediscover tool before a new cell: " + name}
	}
	return nestedIdentity{spec, hash}, nil
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
		out = append(out, map[string]any{"name": name, "inputSchema": id.spec.Schema, "outputSchema": id.spec.OutputSchema, "schema_hash": id.hash, "returns": "MCP: {content, structuredContent?, isError?}; native: {text,isError,timedOut,content?}; image blocks use cell-scoped handle, explicit image(block) emits"})
	}
	b, err := json.Marshal(out)
	if len(b) > codemode.Limits.Arguments {
		return nil, &codemode.Error{Code: "description_limit", Message: "schemas too large; request fewer"}
	}
	return b, err
}
func (c *nestedCell) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
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
	select {
	case c.permits <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case nestedProcessPermits <- struct{}{}:
	case <-ctx.Done():
		<-c.permits
		return nil, ctx.Err()
	}
	c.engine.codeModeMu.Lock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.engine.codeModeMu.Unlock()
		<-c.permits
		<-nestedProcessPermits
		return nil, fmt.Errorf("cell closed")
	}
	c.live++
	c.mu.Unlock()
	c.engine.codeModeMu.Unlock()
	return &nestedLease{parent: c.parent, background: c.background, finish: func() {
		<-c.permits
		<-nestedProcessPermits
		c.engine.codeModeMu.Lock()
		defer c.engine.codeModeMu.Unlock()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.live--
		c.finishLocked()
	}}, nil
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

// finishLocked releases the engine's single-cell slot only after every actual
// host call has ended. A cancelled cell with an unresolved external effect
// therefore remains quarantined even though its machine has exited.
func (c *nestedCell) finishLocked() {
	if c.closed && c.live == 0 {
		c.doneOnce.Do(func() { close(c.done) })
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
		e.codeModeMu.Lock()
		control := e.codeModeDirectOnly[n]
		e.codeModeMu.Unlock()
		if !ok || control || !e.IsToolAllowed(n) || directOnly(n, t) {
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
