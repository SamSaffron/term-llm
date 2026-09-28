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
	record := session.AgentRun{ID: id, ParentSessionID: parent, AgentName: name, Prompt: prompt, Model: model, Status: "queued", TurnsGranted: 20, OwnerInstanceID: m.owner, UpdatedAt: now}
	if resume {
		record.TurnsUsed = existing.TurnsUsed
		record.TurnsGranted = existing.TurnsGranted + 20
	}
	detached, cancelCause := context.WithCancelCause(context.WithoutCancel(ctx))
	cancel := func() { cancelCause(errAgentCancelled) }
	interrupt := func() { cancelCause(context.Canceled) }
	e := &agentEntry{record: record, done: make(chan struct{}), cancel: cancel, interrupt: interrupt, attachment: &agentAttachment{callback: cb, callID: callID}, external: external, originCallID: callID, queued: true, startedAt: now, manager: m}
	e.initial = e.attachment
	if scoped, ok := runner.(interface{ AgentApprovalScope(string) *ApprovalManager }); ok {
		scope := scoped.AgentApprovalScope(parent)
		m.trackApprovals(e, scope)
		detached = ContextWithAgentApprovalScope(detached, scope)
	}
	detached = ContextWithSubagentEventCallback(detached, nil)
	processAgentAdmission.Lock()
	defer processAgentAdmission.Unlock()
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("agent manager is shutting down")
	}
	if previous := m.agents[id]; previous != nil {
		select {
		case <-previous.done:
		default:
			m.mu.Unlock()
			cancel()
			return nil, errors.New("agent is already running")
		}
	}
	if previousAny, exists := processAgentEntries.Load(id); exists {
		previous := previousAny.(*agentEntry)
		select {
		case <-previous.done:
		default:
			m.mu.Unlock()
			cancel()
			return nil, errors.New("agent is already running in this process")
		}
	}
	m.agents[id] = e
	processAgentEntries.Store(id, e)
	m.mu.Unlock()
	m.save(record)
	go m.run(detached, e, runner, depth, model, resume, instructions)
	return e, nil
}

func (m *agentManager) run(ctx context.Context, e *agentEntry, runner SpawnAgentRunner, depth int, model string, resume bool, instructions string) {
	defer func() { close(e.done); m.releaseCollected(e) }()
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		m.finish(e, SpawnAgentRunResult{}, ctx.Err())
		return
	}
	m.mu.Lock()
	if ctx.Err() != nil {
		m.mu.Unlock()
		m.finish(e, SpawnAgentRunResult{}, ctx.Err())
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
	m.finish(e, result, err)
}

func isTypedTurnLimit(err error) bool {
	var limit *llm.MaxTurnsExceededError
	return errors.As(err, &limit)
}

func (m *agentManager) finish(e *agentEntry, result SpawnAgentRunResult, err error) {
	m.mu.Lock()
	e.result = result
	e.err = err
	switch {
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
	case "completed", "turn_limit", "cancelled", "interrupted":
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
	if record.Status == "queued" || record.Status == "running" || record.Status == "awaiting_approval" {
		payload.Duration = time.Since(started).Milliseconds()
	} else {
		payload.Duration = record.UpdatedAt.Sub(started).Milliseconds()
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
