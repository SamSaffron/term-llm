package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
)

const codeModeCancelGrace = 2 * time.Second

// codeModeStore is engine/session memory, not durable conversation storage.
// It is keyed by session ID so a reused Engine cannot leak values into another
// provider request's session.
type codeModeStore struct {
	mu       sync.Mutex
	sessions map[string]map[string]json.RawMessage
}

func newCodeModeStore() *codeModeStore {
	return &codeModeStore{sessions: map[string]map[string]json.RawMessage{}}
}

func (s *codeModeStore) State(sessionID string) json.RawMessage {
	s.mu.Lock()
	values := s.sessions[sessionID]
	next := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		next[key] = append(json.RawMessage(nil), value...)
	}
	s.mu.Unlock()
	raw, err := json.Marshal(next)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

func (s *codeModeStore) Commit(sessionID string, raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > codemode.SessionLimits.StateTotal {
		return 0, fmt.Errorf("state_total_limit: %d bytes exceeds %d", len(raw), codemode.SessionLimits.StateTotal)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return 0, fmt.Errorf("invalid_state: expected a JSON object")
	}
	if strings.Contains(string(raw), "cell-image:") {
		return 0, fmt.Errorf("state_image_handle: image handles are cell-scoped and cannot be stored")
	}
	next := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		if len(value) > codemode.SessionLimits.StateValue {
			return 0, fmt.Errorf("state_value_limit: %s is %d bytes, limit %d", key, len(value), codemode.SessionLimits.StateValue)
		}
		if !json.Valid(value) {
			return 0, fmt.Errorf("invalid_state: value for %s is not valid JSON", key)
		}
		next[key] = append(json.RawMessage(nil), value...)
	}
	s.mu.Lock()
	s.sessions[sessionID] = next
	s.mu.Unlock()
	return len(raw), nil
}

func (s *codeModeStore) Clear(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

func (s *codeModeStore) ClearAll() {
	s.mu.Lock()
	s.sessions = map[string]map[string]json.RawMessage{}
	s.mu.Unlock()
}

// codeModeSessions owns at most one running cell and the most recent terminal
// cell per Engine. A cancelled cell whose host calls still ignore cancellation
// remains quarantined by nestedCell's actual-lifetime leases, not by this map.
type codeModeSessions struct {
	engine *Engine
	store  *codeModeStore

	mu     sync.Mutex
	active *codeModeAsyncCell
	last   *codeModeAsyncCell
	closed bool
}

func newCodeModeSessions(engine *Engine) *codeModeSessions {
	return &codeModeSessions{engine: engine, store: newCodeModeStore()}
}

type codeModeAsyncCell struct {
	sessions   *codeModeSessions
	id         string
	sessionID  string
	capability *nestedCell
	machine    *codemode.Cell
	started    time.Time

	mu              sync.Mutex
	changed         chan struct{}
	parts           []core.ToolContentPart
	pendingBytes    int
	images          int
	imageBytes      int
	truncated       bool
	omittedBytes    int
	omittedImages   int
	status          string
	err             string
	errCode         string
	stateCommitted  bool
	stateBytes      int
	cancelRequested bool
	abandoned       bool
	done            bool
	reported        map[string]string
}

func newCodeModeAsyncCell(s *codeModeSessions, id, sessionID string, capability *nestedCell) *codeModeAsyncCell {
	return &codeModeAsyncCell{
		sessions:   s,
		id:         id,
		sessionID:  sessionID,
		capability: capability,
		started:    time.Now(),
		changed:    make(chan struct{}),
		status:     "running",
		reported:   map[string]string{},
	}
}

func (a *codeModeAsyncCell) notifyLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}

func unresolvedAudit(o core.CodeModeCallOutcome) bool {
	return o.Status == "executing" || o.Outcome == "outcome_unknown"
}

func auditKey(o core.CodeModeCallOutcome) string {
	return o.Status + "|" + o.Outcome + "|" + fmt.Sprint(o.IsError) + "|" + o.Error
}

func (a *codeModeAsyncCell) drainAuditLocked(terminal bool) (calls, unsettled []core.CodeModeCallOutcome) {
	all := a.capability.Outcomes()
	for _, outcome := range all {
		key := auditKey(outcome)
		if a.reported[outcome.ID] != key {
			a.reported[outcome.ID] = key
			calls = append(calls, outcome)
		}
		if terminal && unresolvedAudit(outcome) {
			unsettled = append(unsettled, outcome)
		}
	}
	return calls, unsettled
}

func (a *codeModeAsyncCell) emit(kind string, value json.RawMessage) error {
	switch kind {
	case "text":
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			// text(non-string) emits the JSON representation, matching the
			// prelude's documented behavior.
			text = string(value)
		}
		a.mu.Lock()
		remaining := codemode.SessionLimits.RetainBytes - a.pendingBytes
		if remaining <= 0 {
			a.truncated = true
			a.omittedBytes += len(text)
			a.notifyLocked()
			a.mu.Unlock()
			return nil
		}
		if len(text) > remaining {
			original := len(text)
			text = truncateUTF8(text, remaining)
			a.omittedBytes += original - len(text)
			a.truncated = true
		}
		if text != "" {
			a.pendingBytes += len(text)
			a.parts = append(a.parts, core.ToolContentPart{Type: core.ToolContentPartText, Text: text})
		}
		a.notifyLocked()
		a.mu.Unlock()
		return nil
	case "image":
		a.mu.Lock()
		over := a.images >= codemode.SessionLimits.Images || a.imageBytes >= codemode.SessionLimits.ImageBytes || len(value) > codemode.SessionLimits.ImageBytes
		a.mu.Unlock()
		if over {
			a.dropImage()
			return nil
		}
		part, err := a.capability.Image(value)
		if err != nil {
			return fmt.Errorf("image emission: %w", err)
		}
		bytes := 0
		if part.ImageData != nil {
			bytes = len(part.ImageData.Base64) * 3 / 4
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.images >= codemode.SessionLimits.Images || a.imageBytes+bytes > codemode.SessionLimits.ImageBytes {
			a.truncated = true
			a.omittedImages++
			a.notifyLocked()
			return nil
		}
		a.images++
		a.imageBytes += bytes
		a.parts = append(a.parts, part)
		a.notifyLocked()
		return nil
	default:
		return fmt.Errorf("unknown emission kind %q", kind)
	}
}

func (a *codeModeAsyncCell) dropImage() {
	a.mu.Lock()
	a.truncated = true
	a.omittedImages++
	a.notifyLocked()
	a.mu.Unlock()
}

func truncateUTF8(text string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(text) <= max {
		return text
	}
	cut := text[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func (a *codeModeAsyncCell) finish(result codemode.Result) {
	status := result.Status
	if status == "" {
		status = "failed"
	}
	errMsg := result.Error
	errCode := ""
	committed := false
	size := 0
	a.mu.Lock()
	if a.abandoned {
		status = "cancelled"
		errMsg = "cell abandoned by session teardown"
		errCode = "cancelled"
	} else if status == "completed" {
		n, err := a.sessions.store.Commit(a.sessionID, result.State)
		if err != nil {
			status = "failed"
			errMsg = err.Error()
			errCode = "state_commit_failed"
		} else {
			committed = true
			size = n
		}
	} else if status == "failed" {
		errCode = codeModeScriptErrorCode(errMsg)
		if errCode == "busy" {
			status = "busy"
		}
	} else if status == "cancelled" {
		errCode = "cancelled"
	}
	a.status = status
	a.err = errMsg
	a.errCode = errCode
	a.stateCommitted = committed
	a.stateBytes = size
	a.done = true
	a.notifyLocked()
	a.mu.Unlock()
	a.capability.Close()
	a.sessions.finished(a)
}

func codeModeScriptErrorCode(message string) string {
	switch {
	case strings.Contains(message, "busy: VM capacity"):
		return "busy"
	case strings.Contains(message, "state_total_limit"), strings.Contains(message, "state_value_limit"), strings.Contains(message, "state_limit"),
		strings.Contains(message, "state_image_handle"), strings.Contains(message, "invalid_state"):
		return "state_limit"
	case strings.Contains(message, "image"):
		return "image_error"
	case strings.Contains(message, "call_limit"), strings.Contains(message, "arguments_limit"):
		return "call_limit"
	case strings.Contains(message, "output_limit"), strings.Contains(message, "text_limit"), strings.Contains(message, "queue_limit"):
		return "output_limit"
	case strings.Contains(message, "unawaited_calls"):
		return "unawaited_calls"
	default:
		return "script_error"
	}
}

func (s *codeModeSessions) finished(a *codeModeAsyncCell) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == a {
		s.active = nil
	}
	if !a.abandonedLocked() {
		s.last = a
	}
}

func (a *codeModeAsyncCell) abandonedLocked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.abandoned
}

func resolveOutputTokens(requested int) int {
	if requested <= 0 {
		return codemode.DefaultOutputTokens
	}
	if requested > codemode.MaxOutputTokens {
		return codemode.MaxOutputTokens
	}
	return requested
}

const codeModeOutputSettle = 20 * time.Millisecond

func (a *codeModeAsyncCell) observe(ctx context.Context, yield time.Duration, waitForOutput bool, maxOutputTokens int) core.CodeModeCallResult {
	maxOutputTokens = resolveOutputTokens(maxOutputTokens)
	budget := codeModeOutputBudget{tokens: maxOutputTokens, chars: a.sessions.engine.codeModeOutputLimit(), callID: core.CallIDFromContext(ctx)}
	if yield < 0 {
		yield = 0
	}
	if yield == 0 {
		a.mu.Lock()
		result := a.observationLocked(ctx.Err() == nil, yield, terminalStatus(a), budget)
		a.mu.Unlock()
		return result
	}
	timer := time.NewTimer(yield)
	defer timer.Stop()
	var settle *time.Timer
	var settleC <-chan time.Time
	defer func() {
		if settle != nil {
			settle.Stop()
		}
	}()
	resetSettle := func() {
		if settle == nil {
			settle = time.NewTimer(codeModeOutputSettle)
		} else {
			if !settle.Stop() {
				select {
				case <-settle.C:
				default:
				}
			}
			settle.Reset(codeModeOutputSettle)
		}
		settleC = settle.C
	}
	for {
		a.mu.Lock()
		done := a.done
		hasOutput := len(a.parts) > 0
		changed := a.changed
		a.mu.Unlock()
		if done {
			a.mu.Lock()
			result := a.observationLocked(ctx.Err() == nil, yield, terminalStatus(a), budget)
			a.mu.Unlock()
			return result
		}
		if waitForOutput && hasOutput {
			// Coalesce a short output burst with imminent completion: a script
			// that emits then returns should need only one call.
			resetSettle()
			select {
			case <-changed:
				continue
			case <-settleC:
				a.mu.Lock()
				result := a.observationLocked(ctx.Err() == nil, yield, terminalStatus(a), budget)
				a.mu.Unlock()
				return result
			case <-timer.C:
				a.mu.Lock()
				result := a.observationLocked(ctx.Err() == nil, yield, terminalStatus(a), budget)
				a.mu.Unlock()
				return result
			case <-ctx.Done():
				a.mu.Lock()
				result := a.observationLocked(false, yield, "running", budget)
				a.mu.Unlock()
				return result
			}
		}
		select {
		case <-changed:
		case <-timer.C:
			a.mu.Lock()
			status := "yielded"
			if len(a.parts) > 0 {
				status = "running"
			}
			result := a.observationLocked(ctx.Err() == nil, yield, status, budget)
			a.mu.Unlock()
			return result
		case <-ctx.Done():
			a.mu.Lock()
			result := a.observationLocked(false, yield, "running", budget)
			a.mu.Unlock()
			return result
		}
	}
}

func terminalStatus(a *codeModeAsyncCell) string {
	if a.done {
		return a.status
	}
	if a.cancelRequested {
		return "cancelling"
	}
	return "running"
}

// drainPartsLocked applies the per-observation text budget while keeping the
// remainder queued for a later wait. Images have separate count/byte budgets.
func (a *codeModeAsyncCell) drainPartsLocked(maxBytes int) ([]core.ToolContentPart, bool) {
	remaining := maxBytes
	var parts []core.ToolContentPart
	keep := make([]core.ToolContentPart, 0, len(a.parts))
	limited := false
	deferred := false
	for _, part := range a.parts {
		if deferred {
			keep = append(keep, part)
			limited = true
			continue
		}
		if part.Type == core.ToolContentPartImageData {
			parts = append(parts, part)
			continue
		}
		if remaining <= 0 {
			deferred = true
			keep = append(keep, part)
			limited = true
			continue
		}
		if len(part.Text) <= remaining {
			parts = append(parts, part)
			a.pendingBytes -= len(part.Text)
			remaining -= len(part.Text)
			continue
		}
		head := truncateUTF8(part.Text, remaining)
		if head != "" {
			parts = append(parts, core.ToolContentPart{Type: core.ToolContentPartText, Text: head})
			a.pendingBytes -= len(head)
		}
		if tail := part.Text[len(head):]; tail != "" {
			keep = append(keep, core.ToolContentPart{Type: core.ToolContentPartText, Text: tail})
		}
		limited = true
		deferred = true
		remaining = 0
	}
	a.parts = keep
	return parts, limited
}

func (a *codeModeAsyncCell) observationLocked(drain bool, yield time.Duration, status string, budget codeModeOutputBudget) core.CodeModeCallResult {
	var calls, unsettled []core.CodeModeCallOutcome
	if drain {
		calls, unsettled = a.drainAuditLocked(a.done)
	}
	result := core.CodeModeCallResult{
		CellID:         a.id,
		Status:         status,
		Error:          a.err,
		ErrorCode:      a.errCode,
		StateCommitted: a.stateCommitted,
		StateBytes:     a.stateBytes,

		MaxOutputTokens: budget.tokens,
		OutputTruncated: a.truncated,

		HasMoreOutput:  len(a.parts) > 0,
		OmittedBytes:   a.omittedBytes,
		OmittedImages:  a.omittedImages,
		Calls:          calls,
		UnsettledCalls: unsettled,
		Yield:          yield,
		Elapsed:        time.Since(a.started),
	}
	if drain && len(a.parts) > 0 {
		result.Parts, result.OutputLimited = a.drainPartsLocked(budget.textBytes(result))
	}
	result.HasMoreOutput = len(a.parts) > 0
	result.IsError = result.Status == "failed" || result.Status == "cancelled" || result.Status == "unknown" || result.Status == "busy"
	if !a.done && status == "cancelling" {
		result.IsError = false
	}
	return result
}

func (a *codeModeAsyncCell) awaitDone(ctx context.Context, wait time.Duration) {
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		a.mu.Lock()
		if a.done {
			a.mu.Unlock()
			return
		}
		changed := a.changed
		a.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (a *codeModeAsyncCell) attachMachine(machine *codemode.Cell) {
	a.mu.Lock()
	a.machine = machine
	cancel := a.cancelRequested || a.abandoned
	a.mu.Unlock()
	if cancel {
		machine.Cancel()
	}
}

func (a *codeModeAsyncCell) requestCancel() {
	a.mu.Lock()
	a.cancelRequested = true
	machine := a.machine
	a.mu.Unlock()
	if machine != nil {
		machine.Cancel()
	}
}

func (a *codeModeAsyncCell) abandon() {
	a.mu.Lock()
	a.abandoned = true
	a.cancelRequested = true
	machine := a.machine
	a.mu.Unlock()
	if machine != nil {
		machine.Cancel()
	}
	a.capability.Close()
}

func (s *codeModeSessions) lookup(cellID, sessionID string) *codeModeAsyncCell {
	s.mu.Lock()
	defer s.mu.Unlock()
	match := func(c *codeModeAsyncCell) bool {
		if c == nil || c.id != cellID {
			return false
		}
		return c.sessionID == sessionID
	}
	if match(s.active) {
		return s.active
	}
	if match(s.last) {
		return s.last
	}
	return nil
}

func (s *codeModeSessions) exec(ctx context.Context, code string, yield time.Duration, maxOutputTokens int, send corestream.Sender) (core.CodeModeCallResult, error) {
	if !codemode.Available() {
		return core.CodeModeCallResult{Status: "failed", Error: "code_mode unavailable on browser host", ErrorCode: "unavailable", IsError: true}, nil
	}
	if yield <= 0 {
		yield = codemode.SessionLimits.Yield
	}
	if yield > codemode.SessionLimits.MaxYield {
		yield = codemode.SessionLimits.MaxYield
	}
	if len(code) > codemode.SessionLimits.Code {
		return core.CodeModeCallResult{Status: "failed", Error: fmt.Sprintf("code_limit: %d bytes exceeds %d", len(code), codemode.SessionLimits.Code), ErrorCode: "code_limit", IsError: true}, nil
	}
	sessionID := core.SessionIDFromContext(ctx)
	parent := core.CallIDFromContext(ctx)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return engineClosedResult(), nil
	}
	active := s.active
	s.mu.Unlock()
	if active != nil {
		if active.sessionID != sessionID {
			// Never expose or mutate another session's cell from this call.
			return core.CodeModeCallResult{Status: "busy", Error: "another session's code cell is active; retry after it settles", ErrorCode: "session_mismatch", IsError: true}, nil
		}
		return core.CodeModeCallResult{CellID: active.id, ActiveCellID: active.id, Status: "busy", Error: "a code cell is already running; use wait_script or cancel_script", ErrorCode: "cell_active", IsError: true}, nil
	}

	capability, err := s.engine.admitCodeCellFor(ctx, parent, send, codemode.ExecToolName, true)
	if err != nil {
		code := "code_mode_failed"
		var typed *codemode.Error
		if errors.As(err, &typed) {
			code = typed.Code
		}
		status := "failed"
		if code == "busy" {
			status = "busy"
		}
		return core.CodeModeCallResult{Status: status, Error: err.Error(), ErrorCode: code, IsError: true}, nil
	}

	cellID := "cm_" + core.NewSyntheticToolCallID()
	a := newCodeModeAsyncCell(s, cellID, sessionID, capability)
	state := s.store.State(sessionID)
	// Hold the manager lock across publication and start so CloseCodeMode
	// either rejects this exec or observes the active cell and abandons it.
	// StartCell only spawns the machine goroutine; it cannot call back into the
	// manager before this function returns.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		capability.Close()
		return engineClosedResult(), nil
	}
	if s.active != nil {
		other := s.active.id
		s.mu.Unlock()
		capability.Close()
		return core.CodeModeCallResult{CellID: other, ActiveCellID: other, Status: "busy", Error: "a code cell is already running; use wait_script or cancel_script", ErrorCode: "cell_active", IsError: true}, nil
	}
	s.active = a
	machine, err := codemode.StartCell(ctx, codemode.CellSpec{
		Input:    codemode.Input{Code: code, State: state},
		Names:    capability.Names(),
		Host:     capability,
		Modules:  capability,
		Sink:     a.emit,
		OnFinish: a.finish,
	})
	if err != nil {
		if s.active == a {
			s.active = nil
		}
		s.mu.Unlock()
		capability.Close()
		return core.CodeModeCallResult{CellID: cellID, Status: "failed", Error: err.Error(), ErrorCode: "start_failed", IsError: true}, nil
	}
	s.mu.Unlock()
	a.attachMachine(machine)
	return a.observe(ctx, yield, true, maxOutputTokens), nil
}

func (s *codeModeSessions) wait(ctx context.Context, cellID string, yield time.Duration, maxOutputTokens int) (core.CodeModeCallResult, error) {
	if yield <= 0 {
		yield = codemode.SessionLimits.Yield
	}
	if yield > codemode.SessionLimits.MaxYield {
		yield = codemode.SessionLimits.MaxYield
	}
	sessionID := core.SessionIDFromContext(ctx)
	a := s.lookup(cellID, sessionID)
	if a == nil {
		return unknownCell(cellID), nil
	}
	return a.observe(ctx, yield, true, maxOutputTokens), nil
}

func (s *codeModeSessions) cancel(ctx context.Context, cellID string) (core.CodeModeCallResult, error) {
	sessionID := core.SessionIDFromContext(ctx)
	a := s.lookup(cellID, sessionID)
	if a == nil {
		return unknownCell(cellID), nil
	}
	a.mu.Lock()
	done := a.done
	a.mu.Unlock()
	if !done {
		a.requestCancel()
		a.awaitDone(ctx, codeModeCancelGrace)
	}
	return a.observe(ctx, 0, true, codemode.DefaultOutputTokens), nil
}

func unknownCell(cellID string) core.CodeModeCallResult {
	return core.CodeModeCallResult{CellID: cellID, Status: "unknown", Error: "unknown or expired cell_id; only the active cell and the most recent terminal cell are retained", ErrorCode: "unknown_cell", IsError: true}
}

func (s *codeModeSessions) cancelActive() {
	s.mu.Lock()
	active := s.active
	s.active = nil
	s.last = nil
	s.mu.Unlock()
	if active != nil {
		active.abandon()
	}
}

func (s *codeModeSessions) cancelSession(sessionID string) {
	s.mu.Lock()
	var active *codeModeAsyncCell
	if s.active != nil && s.active.sessionID == sessionID {
		active = s.active
		s.active = nil
	}
	if s.last != nil && s.last.sessionID == sessionID {
		s.last = nil
	}
	s.mu.Unlock()
	if active != nil {
		active.abandon()
	}
	s.store.Clear(sessionID)
}

func (s *codeModeSessions) closeAll() {
	s.mu.Lock()
	s.closed = true
	active := s.active
	s.active = nil
	s.last = nil
	s.mu.Unlock()
	if active != nil {
		active.abandon()
	}
	s.store.ClearAll()
}

func engineClosedResult() core.CodeModeCallResult {
	return core.CodeModeCallResult{Status: "failed", Error: "code mode engine is closed; use a new runtime or session", ErrorCode: "engine_closed", IsError: true}
}

type codeModeController struct {
	engine *Engine
	send   corestream.Sender
}

func (c *codeModeController) ExecCodeMode(ctx context.Context, code string, yield time.Duration, maxOutputTokens int) (core.CodeModeCallResult, error) {
	return c.engine.codeModeSessions.exec(ctx, code, yield, maxOutputTokens, c.send)
}

func (c *codeModeController) WaitCodeMode(ctx context.Context, cellID string, yield time.Duration, maxOutputTokens int) (core.CodeModeCallResult, error) {
	c.rebind(ctx, cellID)
	return c.engine.codeModeSessions.wait(ctx, cellID, yield, maxOutputTokens)
}

func (c *codeModeController) CancelCodeMode(ctx context.Context, cellID string) (core.CodeModeCallResult, error) {
	c.rebind(ctx, cellID)
	return c.engine.codeModeSessions.cancel(ctx, cellID)
}

// ExecCodeMode starts a fresh engine-owned cell. The cell outlives this call's
// request context; only cancel or session teardown stops it.
func (e *Engine) ExecCodeMode(ctx context.Context, code string, yield time.Duration, maxOutputTokens int) (core.CodeModeCallResult, error) {
	if e == nil || e.codeModeSessions == nil {
		return core.CodeModeCallResult{Status: "failed", Error: "code mode session manager unavailable", ErrorCode: "unavailable", IsError: true}, nil
	}
	return e.codeModeSessions.exec(ctx, code, yield, maxOutputTokens, corestream.Sender{})
}

// WaitCodeMode observes new output or completion without ever cancelling.
func (e *Engine) WaitCodeMode(ctx context.Context, cellID string, yield time.Duration, maxOutputTokens int) (core.CodeModeCallResult, error) {
	if e == nil || e.codeModeSessions == nil {
		return core.CodeModeCallResult{CellID: cellID, Status: "unknown", Error: "code mode session manager unavailable", ErrorCode: "unavailable", IsError: true}, nil
	}
	return e.codeModeSessions.wait(ctx, cellID, yield, maxOutputTokens)
}

// CancelCodeMode is the explicit lifecycle stop. It reports unsettled external
// calls as outcome_unknown and leaves their actual-effect leases quarantined.
func (e *Engine) CancelCodeMode(ctx context.Context, cellID string) (core.CodeModeCallResult, error) {
	if e == nil || e.codeModeSessions == nil {
		return core.CodeModeCallResult{CellID: cellID, Status: "unknown", Error: "code mode session manager unavailable", ErrorCode: "unavailable", IsError: true}, nil
	}
	return e.codeModeSessions.cancel(ctx, cellID)
}

// CloseCodeMode cancels the active cell and discards all engine-owned code
// state. It is safe to call from runtime eviction/shutdown and does not wait
// for host calls that ignore their context.
func (e *Engine) CloseCodeMode() {
	if e == nil || e.codeModeSessions == nil {
		return
	}
	e.codeModeSessions.closeAll()
}

// CloseCodeModeRuntime is the narrow runtime-lifecycle adapter: owned engines
// discard state, borrowed engines only cancel cells so the owning session keeps
// its store.
func (e *Engine) CloseCodeModeRuntime(borrowed bool) {
	if borrowed {
		e.CancelCodeModeCells()
		return
	}
	e.CloseCodeMode()
}

// CancelCodeModeCells stops running cells without discarding stored state. It
// is the lifecycle hook for a borrowed engine whose owning session outlives
// the runtime that started the cell.
func (e *Engine) CancelCodeModeCells() {
	if e == nil || e.codeModeSessions == nil {
		return
	}
	e.codeModeSessions.cancelActive()
}

// ResetCodeModeSession drops only one session's state and stops that session's
// active cell. A reused Engine cannot leak cell state into another request.
func (e *Engine) ResetCodeModeSession(sessionID string) {
	if e == nil || e.codeModeSessions == nil {
		return
	}
	e.codeModeSessions.cancelSession(sessionID)
}

func (c *codeModeController) rebind(ctx context.Context, cellID string) {
	if a := c.engine.codeModeSessions.lookup(cellID, core.SessionIDFromContext(ctx)); a != nil {
		a.capability.bindSender(c.send)
	}
}

// InterruptCodeModeSession stops work on explicit human interrupt, unlike a
// cancelled observation/request. Committed store and the terminal receipt stay
// available; another session's cell is never affected. It does not wait for an
// external operation which ignores cancellation.
func (e *Engine) InterruptCodeModeSession(sessionID string) bool {
	if e == nil || e.codeModeSessions == nil {
		return false
	}
	s := e.codeModeSessions
	s.mu.Lock()
	a := s.active
	s.mu.Unlock()
	if a == nil || a.sessionID != sessionID {
		return false
	}
	a.mu.Lock()
	done := a.done
	a.mu.Unlock()
	if done {
		return false
	}
	a.requestCancel()
	return true
}
