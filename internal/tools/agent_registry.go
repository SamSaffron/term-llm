package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// AgentContinuation is implemented by hosts that can resume a persisted child
// and steer a live engine. Ordinary test runners still support spawn/wait/cancel.
type AgentContinuation interface {
	ContinueAgent(context.Context, string, string, string, int, SpawnAgentRunOptions, SubagentEventCallback) (SpawnAgentRunResult, error)
	SteerAgent(string, string) (string, string)
}

type agentEntry struct {
	record   session.AgentRun
	done     chan struct{}
	cancel   context.CancelFunc
	callback SubagentEventCallback
	callID   string
	result   SpawnAgentRunResult
	media    []llm.MediaArtifact
	err      error
	queued   bool
}

type agentManager struct {
	mu       sync.Mutex
	agents   map[string]*agentEntry
	slots    chan struct{}
	store    session.AgentRunStore
	owner    string
	runner   SpawnAgentRunner
	depth    int
	external SubagentEventCallback
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
		return false
	} // Other hosts cannot prove another process's start identity here.
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
	_ = m.store.PutAgentRun(ctx, record)
}

func (m *agentManager) start(ctx context.Context, name, prompt, model, callID string, cb SubagentEventCallback, runner SpawnAgentRunner, depth int, resume bool, instructions string, existing session.AgentRun) *agentEntry {
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
	if parent != "" {
		ctx = llm.ContextWithSessionID(ctx, parent)
	}
	if resume {
		parent = existing.ParentSessionID
	}
	now := time.Now()
	record := session.AgentRun{ID: id, ParentSessionID: parent, AgentName: name, Prompt: prompt, Status: "queued", TurnsGranted: 20, OwnerInstanceID: m.owner, UpdatedAt: now}
	if resume {
		record.TurnsUsed = existing.TurnsUsed
		record.TurnsGranted = existing.TurnsGranted + 20
	}
	detached, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if scoped, ok := runner.(interface{ AgentApprovalScope(string) *ApprovalManager }); ok {
		detached = ContextWithAgentApprovalScope(detached, scoped.AgentApprovalScope(parent))
	}
	detached = ContextWithSubagentEventCallback(detached, nil)
	e := &agentEntry{record: record, done: make(chan struct{}), cancel: cancel, callback: cb, callID: callID, queued: true}
	m.mu.Lock()
	m.agents[id] = e
	m.mu.Unlock()
	m.save(record)
	go m.run(detached, e, runner, depth, model, resume, instructions)
	return e
}

func (m *agentManager) run(ctx context.Context, e *agentEntry, runner SpawnAgentRunner, depth int, model string, resume bool, instructions string) {
	defer close(e.done)
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
	e.record.UpdatedAt = time.Now()
	record := e.record
	m.mu.Unlock()
	m.save(record)
	cb := func(_ string, event SubagentEvent) {
		m.mu.Lock()
		e.record.UpdatedAt = time.Now()
		if event.Type == SubagentEventToolStart {
			e.record.StopReason = event.ToolName
		}
		if event.Type == SubagentEventToolEnd {
			e.record.StopReason = ""
			e.media = append(e.media, event.Media...)
		}
		attached, callID := e.callback, e.callID
		record := e.record
		m.mu.Unlock()
		if attached != nil {
			attached(callID, event)
		}
		if m.external != nil {
			m.external(callID, event)
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
			result, err = continuation.ContinueAgent(ctx, e.record.ID, e.record.AgentName, instructions, depth, opts, cb)
		} else {
			err = errors.New("runner does not support resuming agents")
		}
	} else if extended, ok := runner.(SpawnAgentRunnerWithOptions); ok {
		result, err = extended.RunAgentWithCallbackAndOptions(ctx, e.record.AgentName, e.record.Prompt, depth, e.callID, cb, opts)
	} else {
		result, err = runner.RunAgentWithCallback(ctx, e.record.AgentName, e.record.Prompt, depth, e.callID, cb)
	}
	m.finish(e, result, err)
}

func (m *agentManager) finish(e *agentEntry, result SpawnAgentRunResult, err error) {
	m.mu.Lock()
	e.result = result
	e.err = err
	switch {
	case errors.Is(err, context.Canceled):
		e.record.Status = "cancelled"
	case llm.IsMaxTurnsExceeded(err):
		e.record.Status = "turn_limit"
	case err != nil:
		e.record.Status = "failed"
	default:
		e.record.Status = "completed"
	}
	e.record.StopReason = ""
	e.record.Output = result.Output
	if err != nil {
		e.record.Error = err.Error()
	}
	e.record.UpdatedAt = time.Now()
	e.callback = nil
	record := e.record
	m.mu.Unlock()
	m.save(record)
}

func (m *agentManager) get(ctx context.Context, id, parent string) (session.AgentRun, *agentEntry, error) {
	m.mu.Lock()
	e := m.agents[id]
	var record session.AgentRun
	if e != nil {
		record = e.record
	}
	m.mu.Unlock()
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
	m.mu.Lock()
	for _, e := range m.agents {
		if e.record.ParentSessionID == parent && !seen[e.record.ID] {
			runs = append(runs, e.record)
		}
	}
	m.mu.Unlock()
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

func (m *agentManager) detach(e *agentEntry) {
	m.mu.Lock()
	e.callback = nil
	e.callID = ""
	m.mu.Unlock()
}

func (m *agentManager) attach(e *agentEntry, cb SubagentEventCallback, callID string) {
	m.mu.Lock()
	e.callback = cb
	e.callID = callID
	m.mu.Unlock()
}

func agentOutput(a session.AgentRun) llm.ToolOutput {
	status := a.Status
	result := SpawnAgentResult{AgentName: a.AgentName, AgentID: a.ID, SessionID: a.ID, Status: status, Output: a.Output, Error: a.Error, Duration: 0, TurnsUsed: a.TurnsUsed, LastActivity: a.UpdatedAt}
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

func agentParent(ctx context.Context) string { return llm.SessionIDFromContext(ctx) }

// Shutdown cancels queued work first, then active children, and waits for their
// terminal writes before hosts close the session store.
func (m *agentManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
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
	for _, e := range append(queued, running...) {
		e.cancel()
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
