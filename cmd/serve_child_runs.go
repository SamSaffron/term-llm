package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	childRunAskUserWait  = 10 * time.Minute
	childRunAskUserGrace = 15 * time.Second

	delegatedSessionWriteMessage = "this is a delegated transcript; steer the running subagent or reply in the conversation that started it"
)

func (s *serveServer) sessionIsDelegatedRun(ctx context.Context, sessionID string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || s == nil || s.store == nil {
		return false, nil
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return sessionIsDelegatedChild(sess), nil
}

func sessionIsDelegatedChild(sess *session.Session) bool {
	return sess != nil && strings.TrimSpace(sess.ParentID) != ""
}

type childRunIntervention struct {
	ID          string
	Text        string
	Disposition string
}

type childRunHandle struct {
	srv *serveServer

	childSessionID  string
	parentSessionID string
	callID          string
	agent           string
	taskSummary     string
	startedAt       int64

	mu              sync.Mutex
	pendingAsks     int
	interventions   []childRunIntervention
	cancelledByUser bool
	finished        bool
	endedAt         int64
	status          session.SessionStatus
}

type childRunRegistry struct {
	srv *serveServer

	mu       sync.RWMutex
	byChild  map[string]*childRunHandle
	byParent map[string]map[string]*childRunHandle
}

func newChildRunRegistry(srv *serveServer) *childRunRegistry {
	return &childRunRegistry{
		srv:      srv,
		byChild:  make(map[string]*childRunHandle),
		byParent: make(map[string]map[string]*childRunHandle),
	}
}

func (s *serveServer) ensureChildRuns() *childRunRegistry {
	if s == nil {
		return nil
	}
	s.childRunsOnce.Do(func() { s.childRuns = newChildRunRegistry(s) })
	return s.childRuns
}

func (r *childRunRegistry) lookup(childSessionID string) *childRunHandle {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byChild[strings.TrimSpace(childSessionID)]
}

func (r *childRunRegistry) forParent(parentSessionID string) []*childRunHandle {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	children := r.byParent[strings.TrimSpace(parentSessionID)]
	out := make([]*childRunHandle, 0, len(children))
	for _, child := range children {
		out = append(out, child)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].startedAt < out[j].startedAt })
	return out
}

func (r *childRunRegistry) ChildRunStarted(info childRunInfo) childRunSession {
	if r == nil {
		return nil
	}
	childID := strings.TrimSpace(info.ChildSessionID)
	parentID := strings.TrimSpace(info.ParentSessionID)
	if childID == "" || parentID == "" {
		return nil
	}
	handle := &childRunHandle{
		srv:             r.srv,
		childSessionID:  childID,
		parentSessionID: parentID,
		callID:          strings.TrimSpace(info.CallID),
		agent:           strings.TrimSpace(info.Agent),
		taskSummary:     truncateChildText(strings.TrimSpace(info.Prompt), 240),
		startedAt:       time.Now().UnixMilli(),
	}
	r.mu.Lock()
	r.byChild[childID] = handle
	if r.byParent[parentID] == nil {
		r.byParent[parentID] = make(map[string]*childRunHandle)
	}
	r.byParent[parentID][childID] = handle
	r.mu.Unlock()
	r.srv.publishChildrenChanged(childID, parentID, "subagent_started")
	return handle
}

func (r *childRunRegistry) release(handle *childRunHandle) {
	if r == nil || handle == nil {
		return
	}
	r.mu.Lock()
	if r.byChild[handle.childSessionID] == handle {
		delete(r.byChild, handle.childSessionID)
	}
	if children := r.byParent[handle.parentSessionID]; children != nil {
		if children[handle.childSessionID] == handle {
			delete(children, handle.childSessionID)
		}
		if len(children) == 0 {
			delete(r.byParent, handle.parentSessionID)
		}
	}
	r.mu.Unlock()
}

func (s *serveServer) publishChildrenChanged(childSessionID, parentSessionID, reason string) {
	if s == nil || strings.TrimSpace(parentSessionID) == "" {
		return
	}
	s.publishEvent(serveEventInput{Type: serveEventChildrenChanged, SessionID: childSessionID, ParentSessionID: parentSessionID, Reason: reason})
}

func truncateChildText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// Execute hands the prepared child runtime to the same response executor used
// by POST /v1/responses. The runtime is temporarily attached to sessionMgr so
// ordinary session state, steering, approvals and ask-user handlers address it.
func (h *childRunHandle) Execute(ctx context.Context, env *cmdRunEnvironment, onEvent func(llm.Event) error) (serveRunResult, error) {
	if h == nil || h.srv == nil || env == nil || env.runtime == nil {
		return serveRunResult{}, errors.New("hosted child runtime is unavailable")
	}
	if h.srv.sessionMgr == nil {
		return serveRunResult{}, errors.New("hosted child session manager is unavailable")
	}
	// The local Prompt mode delegates enforcement to the parent; it is not
	// the configured default shown by the web approval controls.
	env.runtime.approvalDefault = h.srv.approvalDefault
	release, err := h.srv.sessionMgr.attachBorrowedRuntime(env.req.SessionID, env.runtime)
	if err != nil {
		return serveRunResult{}, err
	}
	var result serveRunResult
	var executionErr error
	run, err := h.srv.startResponseRun(env.runtime, env.req.Stateful, env.req.ReplaceHistory, env.inputMessages, env.llmReq, env.req.SessionID, startResponseRunOptions{
		parentContext: ctx,
		onRuntimeDone: release,
		onEvent: func(event llm.Event) error {
			h.observeEvent(event)
			if onEvent != nil {
				return onEvent(event)
			}
			return nil
		},
		onResult: func(completed serveRunResult, runErr error) {
			result, executionErr = completed, runErr
		},
	})
	if err != nil {
		release()
		return serveRunResult{}, err
	}
	// startResponseRun now owns and closes the non-stateful runtime.
	env.runtime = nil
	h.srv.publishChildrenChanged(h.childSessionID, h.parentSessionID, "subagent_ready")
	<-run.settled
	return result, executionErr
}

func (h *childRunHandle) observeEvent(event llm.Event) {
	if event.Type != llm.EventSteering || event.SteeringStatus != llm.SteeringCommitted {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.interventions {
		if h.interventions[i].ID == event.SteeringID {
			h.interventions[i].Disposition = tools.InterventionConsumed
			return
		}
	}
}

func (h *childRunHandle) recordIntervention(id, text string) {
	id, text = strings.TrimSpace(id), strings.TrimSpace(text)
	if h == nil || id == "" || text == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range h.interventions {
		if existing.ID == id {
			return
		}
	}
	h.interventions = append(h.interventions, childRunIntervention{
		ID: id, Text: text, Disposition: tools.InterventionQueued,
	})
}

func (h *childRunHandle) rejectIntervention(id string) {
	if h == nil || strings.TrimSpace(id) == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.interventions {
		if h.interventions[i].ID == id {
			h.interventions = append(h.interventions[:i], h.interventions[i+1:]...)
			return
		}
	}
}

func (h *childRunHandle) recordUserCancellation() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.cancelledByUser = true
	h.mu.Unlock()
}

func (h *childRunHandle) AskUser(ctx context.Context, questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
	if h == nil {
		return nil, errors.New("ask_user is not available to this delegated run")
	}
	parentRT := h.parentRuntime()
	if parentRT == nil {
		return nil, errors.New("ask_user is unavailable: the parent conversation is not loaded")
	}
	callID := strings.TrimSpace(llm.CallIDFromContext(ctx))
	if callID == "" {
		return nil, errors.New("ask_user missing tool call id")
	}
	qualified := h.childSessionID + ":" + callID
	origin := serveAskUserOrigin{ChildSessionID: h.childSessionID, ChildAgent: h.agent, ChildTask: h.taskSummary, ParentCallID: h.callID}

	// Event projection prepares the canonical gate on the child runtime before
	// tool execution starts. The parent gets only a qualified alias to that gate,
	// so either surface can answer without creating a second waiter.
	waitRT := h.childRuntime()
	var childPending *servePendingAskUser
	if waitRT == nil {
		waitRT = parentRT
		childPending, _ = parentRT.prepareAskUserFrom(qualified, questions, origin)
	} else {
		childPending, _ = waitRT.prepareAskUser(callID, questions)
	}
	parentPending, parentPrompt := parentRT.aliasAskUser(qualified, childPending, origin)
	defer parentRT.removePendingAskUser(qualified, parentPending)
	if waitRT != parentRT {
		defer waitRT.removePendingAskUser(callID, childPending)
	}

	h.mu.Lock()
	h.pendingAsks++
	h.mu.Unlock()
	h.srv.publishChildrenChanged(h.childSessionID, h.parentSessionID, "subagent_question")
	defer func() {
		h.mu.Lock()
		if h.pendingAsks > 0 {
			h.pendingAsks--
		}
		h.mu.Unlock()
		h.srv.publishChildrenChanged(h.childSessionID, h.parentSessionID, "subagent_question_resolved")
	}()
	wait := childRunAskUserWait
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline) - childRunAskUserGrace; remaining < wait {
			wait = remaining
		}
	}
	if wait <= 0 {
		if childPending.gate.abandon("cancelled-by-agent") {
			return nil, errors.New("not enough time left in this subagent's budget to wait for an answer")
		}
		// An early answer already won the gate; the remaining wait budget must
		// not turn its successful acknowledgement into a discarded answer.
		h.publishAskUserPrompt(parentPending, parentPrompt)
		return childPending.await(ctx)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resumeResponseTimeout := waitRT.pauseForInteractiveWait()
	defer resumeResponseTimeout()
	h.publishAskUserPrompt(parentPending, parentPrompt)
	answers, err := childPending.await(waitCtx)
	if err != nil && waitCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return nil, fmt.Errorf("no answer within %s", wait.Round(time.Second))
	}
	return answers, err
}

func (h *childRunHandle) publishAskUserPrompt(pending *servePendingAskUser, prompt serveAskUserPrompt) {
	if h == nil || h.srv == nil {
		return
	}
	run := h.srv.ensureResponseRuns().activeRun(h.parentSessionID)
	if run == nil {
		return
	}
	payload := map[string]any{"call_id": prompt.CallID, "questions": prompt.Questions, "created_at": prompt.CreatedAt}
	if prompt.Origin != nil {
		payload["origin"] = prompt.Origin
	}
	if run.appendEvent("response.ask_user.prompt", payload) == nil {
		pending.bindRun(run)
	}
}

func (h *childRunHandle) AskUserAvailable() bool { return h != nil && h.parentRuntime() != nil }

func (h *childRunHandle) childRuntime() *serveRuntime {
	if h == nil || h.srv == nil || h.srv.sessionMgr == nil {
		return nil
	}
	rt, _ := h.srv.sessionMgr.Get(h.childSessionID)
	return rt
}

func (h *childRunHandle) parentRuntime() *serveRuntime {
	if h == nil || h.srv == nil || h.srv.sessionMgr == nil {
		return nil
	}
	rt, _ := h.srv.sessionMgr.Get(h.parentSessionID)
	return rt
}

func (h *childRunHandle) Finish(status session.SessionStatus, runErr error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.finished {
		h.mu.Unlock()
		return
	}
	h.finished = true
	h.status = status
	h.endedAt = time.Now().UnixMilli()
	for i := range h.interventions {
		if h.interventions[i].Disposition == tools.InterventionQueued {
			h.interventions[i].Disposition = tools.InterventionUndelivered
		}
	}
	h.mu.Unlock()
	if h.srv != nil {
		h.srv.ensureChildRuns().release(h)
		reason := "subagent_completed"
		if runErr != nil {
			reason = "subagent_failed"
		}
		h.srv.publishChildrenChanged(h.childSessionID, h.parentSessionID, reason)
	}
}

func (h *childRunHandle) Outcome() childRunOutcome {
	if h == nil {
		return childRunOutcome{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	texts := make([]string, 0, len(h.interventions))
	consumed, undelivered := 0, 0
	for _, intervention := range h.interventions {
		texts = append(texts, intervention.Text)
		switch intervention.Disposition {
		case tools.InterventionConsumed:
			consumed++
		case tools.InterventionUndelivered:
			undelivered++
		}
	}
	disposition := ""
	if len(texts) > 0 {
		disposition = tools.InterventionQueued
		switch {
		case consumed == len(texts):
			disposition = tools.InterventionConsumed
		case undelivered == len(texts):
			disposition = tools.InterventionUndelivered
		case consumed > 0 || undelivered > 0:
			disposition = tools.InterventionMixed
		}
	}
	return childRunOutcome{Interventions: texts, Disposition: disposition, CancelledByUser: h.cancelledByUser}
}

type childRunLiveState struct {
	RunID       string                `json:"run_id"`
	Live        bool                  `json:"live"`
	Status      session.SessionStatus `json:"status,omitempty"`
	Steerable   bool                  `json:"steerable"`
	PendingAsks int                   `json:"pending_asks,omitempty"`
	StartedAt   int64                 `json:"started_at,omitempty"`
	EndedAt     int64                 `json:"ended_at,omitempty"`
}

func (h *childRunHandle) state() childRunLiveState {
	if h == nil {
		return childRunLiveState{}
	}
	h.mu.Lock()
	state := childRunLiveState{Live: !h.finished, Status: h.status, PendingAsks: h.pendingAsks, StartedAt: h.startedAt, EndedAt: h.endedAt}
	h.mu.Unlock()
	if run := h.srv.ensureResponseRuns().activeRun(h.childSessionID); run != nil {
		run.mu.Lock()
		state.RunID = run.id
		state.Live = state.Live && run.status == "in_progress"
		run.mu.Unlock()
		state.Steerable = state.Live
	}
	return state
}
