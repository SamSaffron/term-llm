package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/restart"
)

const codeModeCancelGrace = 2 * time.Second

// codeModeSessions owns bounded per-session live and terminal cells. Actual
// external effects retain their session place until the native call returns.
type codeModeSessions struct {
	engine *Engine
	store  *codeModeStore

	mu       sync.Mutex // per-engine admission/reset lock, never a process-wide web-session lock
	sessions map[string]*codeModeSessionCells
	closed   bool
}

func newCodeModeSessions(engine *Engine) *codeModeSessions {
	return &codeModeSessions{engine: engine, store: newCodeModeStore(), sessions: map[string]*codeModeSessionCells{}}
}

type codeModeAsyncCell struct {
	sessions         *codeModeSessions
	id               string
	sessionID        string
	capability       *nestedCell
	owner            *codeModeSessionCells
	versions         map[string]uint64
	stateOverwritten []string
	ended            time.Time
	observed         bool
	cancel           context.CancelFunc
	machine          *codemode.Cell
	started          time.Time

	mu              sync.Mutex
	changed         chan struct{}
	parts           []core.ToolContentPart
	textSeparator   bool // Last retained text lacks a newline, even if already observed.
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
		a.emitText(text)
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
		if a.abandoned {
			return nil
		}
		if a.images >= codemode.SessionLimits.Images || a.imageBytes+bytes > codemode.SessionLimits.ImageBytes || !a.reserveImages(bytes) {
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

// emitText adds the emission separator before retention and observation budgets.
// Draining may split this part anywhere without creating another separator.
func (a *codeModeAsyncCell) emitText(text string) {
	if text == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.abandoned {
		return
	}
	if a.textSeparator && !strings.HasPrefix(text, "\n") {
		text = "\n" + text
	}
	remaining := max(0, codemode.SessionLimits.RetainBytes-a.pendingBytes)
	if len(text) > remaining {
		original := len(text)
		text = truncateUTF8(text, remaining)
		a.omittedBytes += original - len(text)
		a.truncated = true
	}
	if text != "" {
		a.textSeparator = !strings.HasSuffix(text, "\n")
		a.pendingBytes += len(text)
		a.parts = append(a.parts, core.ToolContentPart{Type: core.ToolContentPartText, Text: text})
	}
	a.notifyLocked()
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
	} else if a.cancelRequested {
		status = "cancelled"
		errCode = "cancelled"
	} else if status == "completed" {
		n, overwritten, err := a.sessions.store.Apply(a.sessionID, result.State, a.versions)
		a.stateOverwritten = overwritten
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
		if errCode == "vm_capacity" || errCode == "memory_capacity" {
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
	a.ended = time.Now()
	a.notifyLocked()
	a.mu.Unlock()
	a.capability.Close()
	if a.cancel != nil {
		a.cancel()
	}
	a.sessions.finished(a)
}

func codeModeScriptErrorCode(message string) string {
	if strings.HasPrefix(message, "script_error:") {
		return "script_error"
	}
	switch {
	case strings.Contains(message, "vm_capacity"):
		return "vm_capacity"
	case strings.Contains(message, "memory_capacity"):
		return "memory_capacity"
	case strings.Contains(message, "memory_limit"):
		return "memory_limit"
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
	go func() {
		<-a.capability.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		owner := s.sessions[a.sessionID]
		if owner != a.owner {
			return
		}
		delete(owner.live, a.id)
		if !a.abandonedLocked() {
			owner.terminal = append(owner.terminal, a)
			if len(owner.terminal) > codemode.MaxTerminalCells {
				owner.terminal[0].dropRetained()
				owner.terminal = owner.terminal[1:]
			}
		}
		s.pruneOwnerLocked(a.sessionID, owner)
	}()
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
			a.releaseImages(part)
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
		a.observed = a.done
		calls, unsettled = a.drainAuditLocked(a.done)
	}
	result := core.CodeModeCallResult{
		CellID:               a.id,
		Status:               status,
		Error:                a.err,
		ErrorCode:            a.errCode,
		StateCommitted:       a.stateCommitted,
		StateBytes:           a.stateBytes,
		StateOverwrittenKeys: append([]string(nil), a.stateOverwritten...),

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
	if a.cancel != nil {
		a.cancel()
	}
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
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	if machine != nil {
		machine.Cancel()
	}
	a.capability.Close()
	a.dropRetained()
}

type codeModeSessionCells struct {
	live       map[string]*codeModeAsyncCell
	terminal   []*codeModeAsyncCell
	permits    chan struct{}
	inflight   chan struct{}
	memory     *codemode.Budget
	imageMu    sync.Mutex
	imageBytes int
}

func (s *codeModeSessions) ownerLocked(id string) *codeModeSessionCells {
	o := s.sessions[id]
	if o == nil {
		o = &codeModeSessionCells{live: map[string]*codeModeAsyncCell{}, permits: make(chan struct{}, codemode.SessionToolConcurrency), inflight: make(chan struct{}, codemode.SessionInflightCalls), memory: codemode.NewSessionBudget()}
		s.sessions[id] = o
	}
	return o
}
func (s *codeModeSessions) lookup(cellID, sessionID string) *codeModeAsyncCell {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.sessions[sessionID]
	if o == nil {
		return nil
	}
	if a := o.live[cellID]; a != nil {
		return a
	}
	for _, a := range o.terminal {
		if a.id == cellID {
			return a
		}
	}
	return nil
}
func liveIDs(o *codeModeSessionCells) []string {
	ids := make([]string, 0, len(o.live))
	for id := range o.live {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func (s *codeModeSessions) exec(ctx context.Context, code string, yield time.Duration, maxOutputTokens int, send corestream.Sender) (core.CodeModeCallResult, error) {
	if !codemode.Available() {
		return core.CodeModeCallResult{Status: "failed", Error: "code mode unavailable", ErrorCode: "unavailable", IsError: true}, nil
	}
	if yield < 0 {
		yield = codemode.SessionLimits.Yield
	}
	yield = min(yield, codemode.SessionLimits.MaxYield)
	if len(code) > codemode.SessionLimits.Code {
		return core.CodeModeCallResult{Status: "failed", Error: "code_limit", ErrorCode: "code_limit", IsError: true}, nil
	}
	sessionID := core.SessionIDFromContext(ctx)
	cellID := "cm_" + core.NewSyntheticToolCallID()
	owned, release, admissionErr := restart.Detached(context.WithoutCancel(ctx))
	if admissionErr != nil {
		return core.CodeModeCallResult{Status: "failed", Error: admissionErr.Error(), ErrorCode: "unavailable", IsError: true}, nil
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	cellCtx, cancel := context.WithCancel(owned)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return engineClosedResult(), nil
	}
	owner := s.ownerLocked(sessionID)
	if len(owner.live) >= codemode.MaxLiveCells {
		ids := liveIDs(owner)
		s.mu.Unlock()
		cancel()
		return core.CodeModeCallResult{Status: "busy", Error: "eight live cells already admitted; wait or cancel a live cell", ErrorCode: "session_cell_limit", LiveCellIDs: ids, IsError: true}, nil
	}
	// Serialize admission with teardown; capture contains no external execution.
	capability, err := s.engine.admitCodeCellFor(cellCtx, core.CallIDFromContext(ctx), send, codemode.ExecToolName, true)
	if err != nil {
		s.mu.Unlock()
		cancel()
		var typed *codemode.Error
		code := "permission_denied"
		if errors.As(err, &typed) {
			code = typed.Code
		}
		status := "failed"
		if code == "busy" {
			status = "busy"
		}
		return core.CodeModeCallResult{Status: status, Error: err.Error(), ErrorCode: code, IsError: true}, nil
	}
	capability.sessionPermits = owner.permits
	capability.inflight = owner.inflight
	a := newCodeModeAsyncCell(s, cellID, sessionID, capability)
	a.owner = owner
	a.cancel = cancel
	state, versions := s.store.Snapshot(sessionID)
	a.versions = versions
	if err := s.engine.captureCodeModeAuthority(cellCtx, core.CodeModeCellRef{SessionID: sessionID, CellID: cellID, Ctx: cellCtx}, capability); err != nil {
		s.mu.Unlock()
		capability.Close()
		cancel()
		return core.CodeModeCallResult{Status: "failed", Error: err.Error(), ErrorCode: "permission_denied", IsError: true}, nil
	}
	owner.live[cellID] = a
	machine, err := codemode.StartCell(cellCtx, codemode.CellSpec{Context: cellCtx, Input: codemode.Input{Code: code, State: state}, Memory: owner.memory, Names: capability.Names(), Host: capability, Modules: capability, Sink: a.emit, OnFinish: func(result codemode.Result) { defer release(); a.finish(result) }})
	if err != nil {
		delete(owner.live, cellID)
		s.mu.Unlock()
		capability.Close()
		cancel()
		return core.CodeModeCallResult{CellID: cellID, Status: "failed", Error: err.Error(), ErrorCode: "start_failed", IsError: true}, nil
	}
	started = true
	s.mu.Unlock()
	a.attachMachine(machine)
	result := a.observe(ctx, yield, true, maxOutputTokens)
	if result.Status == "busy" {
		s.mu.Lock()
		result.LiveCellIDs = liveIDs(owner)
		s.mu.Unlock()
	}
	return result, nil
}

func (s *codeModeSessions) wait(ctx context.Context, cellID string, yield time.Duration, maxOutputTokens int) (core.CodeModeCallResult, error) {
	if yield < 0 {
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
	return core.CodeModeCallResult{CellID: cellID, Status: "unknown", Error: "unknown or expired cell_id; up to eight live and eight recent terminal cells per session are retained", ErrorCode: "unknown_cell", IsError: true}
}

func (s *codeModeSessions) cancelActive() {
	s.mu.Lock()
	var cells []*codeModeAsyncCell
	for id, o := range s.sessions {
		for _, a := range o.live {
			cells = append(cells, a)
		}
		for _, a := range o.terminal {
			a.dropRetained()
		}
		o.terminal = nil
		s.pruneOwnerLocked(id, o)
	}
	s.mu.Unlock()
	for _, a := range cells {
		a.abandon()
	}
}
func (s *codeModeSessions) cancelSession(sessionID string) {
	s.mu.Lock()
	o := s.sessions[sessionID]
	var cells []*codeModeAsyncCell
	if o != nil {
		for _, a := range o.live {
			cells = append(cells, a)
		}
		for _, a := range o.terminal {
			a.dropRetained()
		}
		o.terminal = nil
	}
	for _, a := range cells {
		a.abandon()
	}
	s.store.Clear(sessionID)
	if o != nil {
		s.pruneOwnerLocked(sessionID, o)
	}
	s.mu.Unlock()
}
func (s *codeModeSessions) pruneOwnerLocked(id string, o *codeModeSessionCells) {
	if len(o.live) == 0 && len(o.terminal) == 0 {
		delete(s.sessions, id)
	}
}
func (s *codeModeSessions) closeAll() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancelActive()
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

// CloseCodeMode cancels all owned cells and discards all engine-owned code
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
// cells. A reused Engine cannot leak cell state into another request.
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
	var cells []*codeModeAsyncCell
	if o := s.sessions[sessionID]; o != nil {
		for _, a := range o.live {
			cells = append(cells, a)
		}
	}
	s.mu.Unlock()
	for _, a := range cells {
		a.requestCancel()
	}
	return len(cells) > 0
}
