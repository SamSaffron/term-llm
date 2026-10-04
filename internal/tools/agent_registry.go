package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
	"github.com/samsaffron/term-llm/internal/session"
)

// AgentContinuation is implemented by hosts that can resume a persisted child
// and steer a live engine. Ordinary test runners still support spawn/wait/cancel.
type AgentContinuation interface {
	ContinueAgent(context.Context, string, string, string, int, string, SpawnAgentRunOptions, SubagentEventCallback) (SpawnAgentRunResult, error)
	SteerAgent(string, string) (string, string)
}

type agentAttachment struct {
	callback SubagentEventCallback
	callID   string
	inFlight sync.WaitGroup
}

type agentEntry struct {
	record       session.AgentRun
	prior        *session.AgentRun
	instructions string
	done         chan struct{}
	cancel       context.CancelFunc
	interrupt    context.CancelFunc
	attachment   *agentAttachment
	initial      *agentAttachment
	external     SubagentEventCallback
	originCallID string
	result       SpawnAgentRunResult
	startedAt    time.Time
	media        []llm.MediaArtifact
	currentTool  string
	err          error
	queued       bool
	shutdown     bool
	manager      *agentManager
}

type agentManager struct {
	mu       sync.Mutex
	agents   map[string]*agentEntry
	slots    chan struct{}
	store    session.AgentRunStore
	owner    string
	draining bool
	runner   SpawnAgentRunner
	depth    int
}

var processAgentEntries sync.Map // agent_id -> *agentEntry, across host-owned tool registries
var processAgentAdmission sync.Mutex

var errAgentCancelled = errors.New("agent cancelled by user")

// AgentCancelled reports cancellation propagated by cancel_agent, not host exit.
func AgentCancelled(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errAgentCancelled)
}

func newAgentManager(config SpawnConfig) *agentManager {
	return &agentManager{agents: make(map[string]*agentEntry), slots: make(chan struct{}, config.MaxParallel), owner: processAgentOwner()}
}

var processStartedAt = time.Now().UnixNano()

func processAgentOwner() string {
	host, _ := os.Hostname()
	start := strconv.FormatInt(processStartedAt, 10)
	if runtime.GOOS == "linux" {
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", os.Getpid())); err == nil {
			if i := strings.LastIndexByte(string(stat), ')'); i >= 0 {
				fields := strings.Fields(string(stat)[i+1:])
				if len(fields) > 19 {
					start = fields[19]
				} // field 22 (starttime), after pid and comm.
			}
		}
	}
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), start)
}

// ownerTerminated requires proof: never infer death from a stale timestamp.
func ownerTerminated(owner string) bool {
	host, _ := os.Hostname()
	parts := strings.Split(owner, ":")
	if len(parts) != 3 || parts[0] != host {
		return false
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid <= 0 {
		return false
	}
	if runtime.GOOS != "linux" {
		return ownerPIDTerminated(pid)
	} // Non-Linux Unix can prove ESRCH, but not process start identity.
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(stat), ')')
	if i < 0 {
		return false
	}
	fields := strings.Fields(string(stat)[i+1:])
	return len(fields) > 19 && fields[19] != parts[2]
}

func (m *agentManager) save(record session.AgentRun) {
	if m.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.PutAgentRun(ctx, record); err != nil {
		runtimeoutput.Logf("agent run %s status persistence failed: %v", record.ID, err)
	}
}

func (m *agentManager) start(ctx context.Context, name, prompt, model, callID string, cb SubagentEventCallback, external SubagentEventCallback, runner SpawnAgentRunner, depth int, resume bool, instructions string, existing session.AgentRun) (*agentEntry, error) {
	id := existing.ID
	if id == "" {
		id = session.NewID()
	}
	parent := llm.SessionIDFromContext(ctx)
	if parent == "" {
		if provider, ok := runner.(interface{ ParentAgentSessionID() string }); ok {
			parent = provider.ParentAgentSessionID()
		}
	}
	if resume {
		parent = existing.ParentSessionID
	}
	if parent != "" {
		ctx = llm.ContextWithSessionID(ctx, parent)
	}
	now := time.Now()
	record := session.AgentRun{ID: id, ParentSessionID: parent, AgentName: name, Prompt: prompt, Model: model, Status: "queued", TurnsGranted: DefaultSubagentMaxTurns, OwnerInstanceID: m.owner, UpdatedAt: now}
	if resume {
		record.TurnsUsed = existing.TurnsUsed
		record.TurnsGranted = existing.TurnsGranted + DefaultSubagentMaxTurns
	}
	detached, cancelCause := context.WithCancelCause(context.WithoutCancel(ctx))
	cancel := func() { cancelCause(errAgentCancelled) }
	interrupt := func() { cancelCause(context.Canceled) }
	e := &agentEntry{record: record, done: make(chan struct{}), cancel: cancel, interrupt: interrupt, attachment: &agentAttachment{callback: cb, callID: callID}, external: external, originCallID: callID, queued: true, startedAt: now, manager: m}
	if resume {
		e.prior = &existing
		e.instructions = instructions
	}
	e.initial = e.attachment
	if scoped, ok := runner.(interface{ AgentApprovalScope(string) *ApprovalManager }); ok {
		scope := scoped.AgentApprovalScope(parent)
		m.trackApprovals(e, scope)
		detached = ContextWithAgentApprovalScope(detached, scope)
	}
	detached = ContextWithSubagentEventCallback(detached, nil)
	// The child outlives this tool call, whose reload operation is released
	// when it returns. Take the child's own ownership now, before launching it;
	// an inherited, already-released operation rejects the child's runner.
	detached, releaseReload, reloadErr := restart.Detached(detached)
	if reloadErr != nil {
		cancel()
		return nil, fmt.Errorf("agent not started: %w", reloadErr)
	}
	if errors.Is(context.Cause(detached), restart.ErrInterrupt) {
		// A reload is already interrupting work; this child would be born
		// interrupted and look resumable in a loop. Refuse it instead.
		cancel()
		releaseReload()
		return nil, errors.New("agent not started: the host is restarting; retry after it reloads")
	}
	abort := func(message string) (*agentEntry, error) {
		m.mu.Unlock()
		cancel()
		releaseReload()
		return nil, errors.New(message)
	}
	processAgentAdmission.Lock()
	defer processAgentAdmission.Unlock()
	m.mu.Lock()
	if m.draining {
		return abort("agent manager is shutting down")
	}
	// The spawning turn may already have been stopped; its stop hook has then
	// already swept this parent's children and would miss this one.
	if ctx.Err() != nil {
		return abort("agent not started: the parent turn was stopped")
	}
	if previous := m.agents[id]; previous != nil {
		select {
		case <-previous.done:
		default:
			return abort("agent is already running")
		}
	}
	if previousAny, exists := processAgentEntries.Load(id); exists {
		previous := previousAny.(*agentEntry)
		select {
		case <-previous.done:
		default:
			return abort("agent is already running in this process")
		}
	}
	m.agents[id] = e
	processAgentEntries.Store(id, e)
	m.mu.Unlock()
	m.save(record)
	go func() {
		defer releaseReload()
		m.run(detached, e, runner, depth, model, resume, instructions)
	}()
	if ctx.Err() != nil {
		// Stopped between the check above and registration: the stop hook's
		// sweep may have run before this entry was visible to it.
		m.mu.Lock()
		e.shutdown = true
		m.mu.Unlock()
		interrupt()
	}
	return e, nil
}

func (m *agentManager) run(ctx context.Context, e *agentEntry, runner SpawnAgentRunner, depth int, model string, resume bool, instructions string) {
	defer func() { close(e.done); m.releaseCollected(e) }()
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		m.finish(e, SpawnAgentRunResult{}, agentStopError(ctx, ctx.Err()))
		return
	}
	m.mu.Lock()
	if ctx.Err() != nil {
		m.mu.Unlock()
		m.finish(e, SpawnAgentRunResult{}, agentStopError(ctx, ctx.Err()))
		return
	}
	e.queued = false
	e.record.Status = "running"
	e.record.Started = true
	e.record.UpdatedAt = time.Now()
	record := e.record
	m.mu.Unlock()
	m.save(record)
	cb := func(eventCallID string, event SubagentEvent) {
		m.mu.Lock()
		e.record.UpdatedAt = time.Now()
		if event.Type == SubagentEventUsage && event.CountsTurn && eventCallID == e.originCallID {
			e.record.TurnsUsed++
		}
		if event.Type == SubagentEventToolStart {
			e.currentTool = event.ToolName
		}
		if event.Type == SubagentEventToolEnd {
			e.currentTool = ""
			e.media = append(e.media, event.Media...)
		}
		attachment := e.attachment
		if attachment != nil && attachment.callback != nil {
			attachment.inFlight.Add(1)
		}
		record := e.record
		m.mu.Unlock()
		if attachment != nil && attachment.callback != nil {
			func() {
				defer attachment.inFlight.Done()
				mappedID := attachment.callID + strings.TrimPrefix(eventCallID, e.originCallID)
				attachment.callback(mappedID, event)
			}()
		}
		if e.external != nil {
			e.external(eventCallID, event)
		}
		if event.Type == SubagentEventToolEnd || event.Type == SubagentEventDone {
			m.save(record)
		}
	}
	var result SpawnAgentRunResult
	var err error
	opts := SpawnAgentRunOptions{ModelOverride: model, ChildSessionID: e.record.ID}
	if resume {
		if continuation, ok := runner.(AgentContinuation); ok {
			result, err = continuation.ContinueAgent(ctx, e.record.ID, e.record.AgentName, instructions, depth, e.originCallID, opts, cb)
		} else {
			err = errors.New("runner does not support resuming agents")
		}
	} else if extended, ok := runner.(SpawnAgentRunnerWithOptions); ok {
		result, err = extended.RunAgentWithCallbackAndOptions(ctx, e.record.AgentName, e.record.Prompt, depth, e.originCallID, cb, opts)
	} else {
		result, err = runner.RunAgentWithCallback(ctx, e.record.AgentName, e.record.Prompt, depth, e.originCallID, cb)
	}
	m.finish(e, result, agentStopError(ctx, err))
}

// agentStopError attributes a stop caused by a reload's grace-period
// cancellation, so the run ends "interrupted" (resumable after the restart)
// rather than looking like a user's cancel_agent.
func agentStopError(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, restart.ErrInterrupt) || !errors.Is(context.Cause(ctx), restart.ErrInterrupt) {
		return err
	}
	return fmt.Errorf("%w: %w", restart.ErrInterrupt, err)
}

func isTypedTurnLimit(err error) bool {
	var limit *llm.MaxTurnsExceededError
	return errors.As(err, &limit)
}

func (m *agentManager) finish(e *agentEntry, result SpawnAgentRunResult, err error) {
	m.mu.Lock()
	e.result = result
	e.err = err
	var admission *AgentRunAdmissionError
	if e.prior != nil && errors.As(err, &admission) {
		// The child never entered execution: restore the resumable record rather
		// than making a transient admission failure terminal.
		e.record = *e.prior
		record := e.record
		m.mu.Unlock()
		m.save(record)
		return
	}
	switch {
	case errors.Is(err, restart.ErrInterrupt):
		e.record.Status = "interrupted"
	case errors.Is(err, context.Canceled) && e.shutdown:
		e.record.Status = "interrupted"
	case errors.Is(err, context.Canceled):
		e.record.Status = "cancelled"
	case isTypedTurnLimit(err):
		e.record.Status = "turn_limit"
	case err != nil:
		e.record.Status = "failed"
	default:
		e.record.Status = "completed"
	}
	e.record.StopReason = e.record.Status
	e.currentTool = ""
	e.record.Output = result.Output
	if err != nil {
		e.record.Error = err.Error()
	}
	e.record.UpdatedAt = time.Now()
	record := e.record
	m.mu.Unlock()
	m.save(record)
}

func (m *agentManager) get(ctx context.Context, id, parent string) (session.AgentRun, *agentEntry, error) {
	var e *agentEntry
	if value, ok := processAgentEntries.Load(id); ok {
		e = value.(*agentEntry)
	}
	var record session.AgentRun
	if e != nil {
		e.manager.mu.Lock()
		record = e.record
		record.CurrentTool = e.currentTool
		e.manager.mu.Unlock()
	}
	if e == nil && m.store != nil {
		var err error
		record, err = m.store.GetAgentRun(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return record, nil, fmt.Errorf("agent not found")
			}
			return record, nil, err
		}
	}
	if record.ID == "" || record.ParentSessionID != parent {
		return record, nil, fmt.Errorf("agent not found for this parent session")
	}
	if e == nil && (record.Status == "queued" || record.Status == "running" || record.Status == "awaiting_approval") {
		if record.OwnerInstanceID == m.owner || ownerTerminated(record.OwnerInstanceID) {
			record.Status = "interrupted"
		} else {
			record.Status = "running_elsewhere"
		}
	}
	return record, e, nil
}

func (m *agentManager) snapshot(ctx context.Context, parent string) ([]session.AgentRun, error) {
	var runs []session.AgentRun
	if m.store != nil {
		var err error
		runs, err = m.store.ListAgentRuns(ctx, parent)
		if err != nil {
			return nil, err
		}
	}
	seen := make(map[string]bool)
	for i := range runs {
		seen[runs[i].ID] = true
		runs[i], _, _ = m.get(ctx, runs[i].ID, parent)
	}
	processAgentEntries.Range(func(_, value any) bool {
		e := value.(*agentEntry)
		e.manager.mu.Lock()
		record := e.record
		record.CurrentTool = e.currentTool
		e.manager.mu.Unlock()
		if record.ParentSessionID == parent && !seen[record.ID] {
			runs = append(runs, record)
		}
		return true
	})
	return runs, nil
}

func (m *agentManager) wait(ctx context.Context, e *agentEntry, budget time.Duration) bool {
	if budget == 0 {
		select {
		case <-e.done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-e.done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (m *agentManager) detachInitial(e *agentEntry) {
	e.manager.mu.Lock()
	initial := e.initial
	e.manager.mu.Unlock()
	if initial != nil {
		m.detach(e, initial)
	}
}

func (m *agentManager) detach(e *agentEntry, attached *agentAttachment) {
	e.manager.mu.Lock()
	if e.attachment == attached {
		e.attachment = nil
	}
	e.manager.mu.Unlock()
	if attached != nil {
		attached.inFlight.Wait()
	}
}

func (m *agentManager) attach(e *agentEntry, cb SubagentEventCallback, callID string) *agentAttachment {
	attached := &agentAttachment{callback: cb, callID: callID}
	e.manager.mu.Lock()
	e.attachment = attached
	e.manager.mu.Unlock()
	return attached
}

func agentOutput(a session.AgentRun) llm.ToolOutput {
	status := a.Status
	result := SpawnAgentResult{AgentName: a.AgentName, AgentID: a.ID, SessionID: a.ID, Status: status, Output: a.Output, Error: a.Error, Duration: 0, TurnsUsed: a.TurnsUsed, LastActivity: a.UpdatedAt, CurrentTool: a.CurrentTool}
	if status == "failed" {
		result.Type = string(ErrExecutionFailed)
	}
	switch status {
	case "queued", "running", "awaiting_approval", "running_elsewhere":
		result.Resumable = false
		result.Next = fmt.Sprintf("wait_agent({\"agent_ids\":[%q]})", a.ID)
	case "completed":
		result.Resumable = true
		result.Next = "result is final; use continue_agent only if you have follow-up work"
	case "turn_limit", "cancelled", "interrupted":
		result.Resumable = true
		result.Next = fmt.Sprintf("continue_agent({\"agent_id\":%q,\"instructions\":\"continue\"})", a.ID)
	}
	data, _ := json.Marshal(result)
	return llm.ToolOutput{Content: string(data), IsError: status == "failed"}
}

func (m *agentManager) output(record session.AgentRun, e *agentEntry) llm.ToolOutput {
	out := agentOutput(record)
	if e == nil {
		return out
	}
	e.manager.mu.Lock()
	result := e.result
	runErr := e.err
	instructions := e.instructions
	media := append([]llm.MediaArtifact(nil), e.media...)
	started := e.startedAt
	e.manager.mu.Unlock()
	var payload SpawnAgentResult
	if json.Unmarshal([]byte(out.Content), &payload) != nil {
		return out
	}
	payload.Interventions = result.Interventions
	payload.InterventionDisposition = result.InterventionDisposition
	payload.CancelledByUser = result.CancelledByUser
	var admission *AgentRunAdmissionError
	if errors.As(runErr, &admission) {
		payload.Error = runErr.Error()
		if e.prior != nil {
			payload.Next = fmt.Sprintf("retry continue_agent({\"agent_id\":%q,\"instructions\":%q}); the agent did not run", record.ID, instructions)
		} else {
			// A fresh spawn that never started is recorded failed, which
			// continue_agent refuses; spawning again is the only retry.
			payload.Resumable = false
			payload.Next = "the agent did not start; call spawn_agent again"
		}
		out.IsError = true
	}
	if record.Status == "queued" || record.Status == "running" || record.Status == "awaiting_approval" {
		payload.Duration = time.Since(started).Milliseconds()
	} else {
		payload.Duration = record.UpdatedAt.Sub(started).Milliseconds()
	}
	if admission != nil {
		payload.Duration = 0
	}
	out.Content = marshalAgentResult(payload)
	out.Media = llm.NormalizeMedia(media, nil)
	return out
}

func marshalAgentResult(result SpawnAgentResult) string {
	data, _ := json.Marshal(result)
	return string(data)
}

func agentParent(ctx context.Context) string { return llm.SessionIDFromContext(ctx) }

// releaseCollected drops process-wide references after the terminal record is
// durable. Without a store the entry must remain discoverable for list/continue.
func (m *agentManager) releaseCollected(e *agentEntry) {
	select {
	case <-e.done:
	default:
		return
	}
	m.mu.Lock()
	if !agentTerminal(e.record.Status) || e.record.CollectedAt.IsZero() {
		m.mu.Unlock()
		return
	}
	if m.store != nil {
		if m.agents[e.record.ID] == e {
			delete(m.agents, e.record.ID)
		}
		processAgentEntries.CompareAndDelete(e.record.ID, e)
		e.attachment = nil
		e.initial = nil
		e.external = nil
		e.media = nil
		e.result = SpawnAgentRunResult{}
	}
	m.mu.Unlock()
}

func (m *agentManager) outstandingIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, e := range m.agents {
		select {
		case <-e.done:
		default:
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (m *agentManager) Drain(ctx context.Context) error {
	m.mu.Lock()
	m.draining = true
	var entries []*agentEntry
	for _, e := range m.agents {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		select {
		case <-e.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *agentManager) gracefulWait(ctx context.Context, entries []*agentEntry, grace time.Duration) {
	if len(entries) == 0 {
		return
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for _, e := range entries {
		select {
		case <-e.done:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Shutdown cancels queued work first, then active children, and waits for their
// terminal writes before hosts close the session store.
func (m *agentManager) Shutdown(ctx context.Context) error { return m.shutdown(ctx, true) }

// CancelDescendants propagates a user's cancel_agent request to nested runs.
func (m *agentManager) CancelDescendants(ctx context.Context) error { return m.shutdown(ctx, false) }

func (m *agentManager) shutdown(ctx context.Context, interrupted bool) error {
	m.mu.Lock()
	m.draining = true
	var queued, running []*agentEntry
	for _, e := range m.agents {
		select {
		case <-e.done:
			continue
		default:
		}
		if e.queued {
			queued = append(queued, e)
		} else {
			running = append(running, e)
		}
	}
	m.mu.Unlock()
	for _, e := range queued {
		e.manager.mu.Lock()
		e.shutdown = interrupted
		e.manager.mu.Unlock()
		if interrupted && e.interrupt != nil {
			e.interrupt()
		} else {
			e.cancel()
		}
	}
	// Give active children a short chance to finish naturally; queued work never
	// acquires a slot during shutdown.
	m.gracefulWait(ctx, running, 250*time.Millisecond)
	m.mu.Lock()
	for _, e := range running {
		select {
		case <-e.done:
		default:
			e.shutdown = interrupted
		}
	}
	m.mu.Unlock()
	for _, e := range running {
		if interrupted && e.interrupt != nil {
			e.interrupt()
		} else {
			e.cancel()
		}
	}
	for _, e := range append(queued, running...) {
		select {
		case <-e.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// collect marks a terminal run as delivered to its parent. Delivery happens
// through spawn_agent, continue_agent or wait_agent, whichever first returns
// the terminal result; afterwards the process entry can be released.
func (m *agentManager) collect(ctx context.Context, record session.AgentRun, e *agentEntry) (session.AgentRun, error) {
	if !agentTerminal(record.Status) {
		return record, nil
	}
	if record.CollectedAt.IsZero() {
		record.CollectedAt = time.Now()
	}
	if e != nil {
		e.manager.mu.Lock()
		e.record.CollectedAt = record.CollectedAt
		e.manager.mu.Unlock()
	}
	// A detached entry may belong to a prior turn whose store has already
	// closed. Collect through this turn's live store, never the old owner.
	if m.store != nil {
		collectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := m.store.CollectAgentRun(collectCtx, record.ID, record.CollectedAt)
		cancel()
		if err != nil {
			return record, err
		}
	}
	return record, nil
}

// InterruptAgentsForParent interrupts every queued or running agent spawned by
// parent, in any registry of this process, and waits for them to stop until
// ctx ends. Hosts call it when the user stops the parent's turn: detached
// children must not keep working (and spending) after an explicit stop. They
// finish as "interrupted" and remain resumable with continue_agent.
func InterruptAgentsForParent(ctx context.Context, parent string) []string {
	if parent == "" {
		return nil
	}
	var entries []*agentEntry
	processAgentEntries.Range(func(_, value any) bool {
		e := value.(*agentEntry)
		select {
		case <-e.done:
			return true
		default:
		}
		e.manager.mu.Lock()
		match := e.record.ParentSessionID == parent
		if match {
			e.shutdown = true
		}
		e.manager.mu.Unlock()
		if match {
			entries = append(entries, e)
		}
		return true
	})
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.record.ID)
		e.interrupt()
	}
	for _, e := range entries {
		select {
		case <-e.done:
		case <-ctx.Done():
			sort.Strings(ids)
			return ids
		}
	}
	sort.Strings(ids)
	return ids
}

// deliver renders a spawn_agent/continue_agent result. A run that finished
// within the wait budget is collected here, so its entry (output and media)
// is released instead of being retained for the life of the process, and a
// later wait_agent does not deliver the same media again.
func (m *agentManager) deliver(ctx context.Context, record session.AgentRun, e *agentEntry) llm.ToolOutput {
	if agentTerminal(record.Status) {
		if collected, err := m.collect(ctx, record, e); err == nil {
			record = collected
		} else {
			runtimeoutput.Logf("agent run %s collection failed: %v", record.ID, err)
		}
	}
	out := m.output(record, e)
	if e != nil && agentTerminal(record.Status) {
		e.manager.releaseCollected(e)
	}
	return out
}
