package cmd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

// prepareUIRuntime is an owning-surface operation, never a hydration hook.
// Election/waiting precedes operation ownership. The old idle runtime remains
// installed until tools, persistence, and history are ready to publish together.
func (s *serveServer) prepareUIRuntime(ctx context.Context, request serveRuntimeRequest, binding serveWorkspaceBinding) (*serveRuntime, error) {
	if !request.RefreshInputs || request.SessionID == "" {
		return nil, fmt.Errorf("session input preparation requires a durable UI conversation")
	}
	refresher, ok := session.AsSessionInputRefresher(s.store)
	if !ok {
		return nil, fmt.Errorf("session store does not support input refresh")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request.RuntimeDir = binding.RuntimeDir
	request.Agent = s.requestedRuntimeAgent(ctx, request.SessionID, request.Agent)
	var ticket *sessionInputTicket
	var err error
	if request.fresh {
		ticket, err = processSessionInputs.acquireFresh(ctx, s.store, request.SessionID, inputBinding(request.Agent, request.RuntimeDir))
	} else {
		ticket, err = processSessionInputs.acquire(ctx, s.store, request.SessionID, inputBinding(request.Agent, request.RuntimeDir))
	}
	if err != nil {
		return nil, err
	}
	defer ticket.fail()
	request.Inputs = ticket.selected()
	// A ready selection is not an idle-only operation. Follow-ups and side
	// activity must reach their normal admission paths without preparing again.
	// Later execution still verifies runtime identity under the admission pin.
	if !request.fresh && request.Inputs != nil {
		m := s.sessionMgr
		m.mu.Lock()
		current := m.sessions[request.SessionID]
		_, reserved := m.reserved.Load(request.SessionID)
		if !m.closed && !reserved && m.creating[request.SessionID] == nil && ticket.current() && runtimeHasSelectedInputs(current, request.Inputs) {
			current.Touch()
			m.mu.Unlock()
			return current, nil
		}
		m.mu.Unlock()
	}
	old, release, err := s.sessionMgr.lockIdleMetadataMutation(request.SessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	if !ticket.current() {
		return nil, errServeSessionBusy
	}
	if old != nil && request.Inputs != nil && old.inputs.Load() == request.Inputs {
		old.Touch()
		return old, nil
	}
	if s.ensureResponseRuns().activeRunID(request.SessionID) != "" {
		return nil, errServeSessionBusy
	}
	// Reserve capacity before constructing or committing. All installers account
	// for this slot, so a disconnect after commit cannot make publication lose it.
	m := s.sessionMgr
	var evicted *serveRuntime
	if old == nil {
		m.mu.Lock()
		evicted, err = m.makeRoomForNewSessionLocked()
		if err == nil {
			m.preparationSlots++
		}
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
		defer func() { m.mu.Lock(); m.preparationSlots--; m.mu.Unlock() }()
	}
	if evicted != nil {
		defer m.retireRuntime(evicted)
	}
	if old != nil && !request.fresh {
		request.Provider = runtimeProviderKey(old)
		request.Model = old.defaultModel
		if old.settings != nil {
			prior := *old.settings
			prior.Search, prior.MCP, prior.MaxTurns = old.search, old.mcpSetting, old.maxTurns
			request.settings = &prior
			request.agentSkills = &old.agentSkills
		}
		mode := old.approvalDefault
		if old.toolMgr != nil {
			mode = old.toolMgr.ApprovalMgr.RequestedApprovalMode()
		}
		request.approvalMode = &mode
	} else if persisted, getErr := s.store.Get(ctx, request.SessionID); getErr != nil {
		return nil, getErr
	} else if persisted != nil && !request.fresh {
		request.Provider = resolveSessionProviderKey(s.cfgRef, persisted)
		request.Model = persisted.Model
	}
	candidate, err := s.createRequestRuntime(ctx, request)
	if err != nil {
		return nil, err
	}
	if candidate.store == nil {
		candidate.store = s.store
	}
	published := false
	committed := false
	defer func() {
		if !published {
			candidate.Close()
			if committed && old != nil {
				old.retiredInputs.Store(true)
				m.mu.Lock()
				if m.sessions[request.SessionID] == old {
					delete(m.sessions, request.SessionID)
				}
				m.mu.Unlock()
				release()
				m.retireRuntime(old)
			}
		}
	}()
	if old != nil && !request.fresh {
		candidate.search = old.search
		candidate.maxTurns = old.maxTurns
		candidate.forceExternalSearch = old.forceExternalSearch
		candidate.autoCompact = old.autoCompact
		candidate.approvalDefault = old.approvalDefault
		candidate.yoloMode = old.yoloMode
		if candidate.toolMgr != nil && old.toolMgr != nil {
			candidate.toolMgr.ApprovalMgr.SetApprovalMode(old.toolMgr.ApprovalMgr.RequestedApprovalMode())
		}
	}
	if err = s.ensurePersistedSessionForProjectBinding(ctx, request.SessionID, candidate, request.Model); err != nil {
		return nil, err
	}
	if err = s.bindResolvedWorkspace(ctx, request.SessionID, candidate, binding); err != nil {
		return nil, err
	}
	if err = s.ensureRuntimeBaseDirForSession(ctx, request.SessionID, candidate); err != nil {
		return nil, err
	}
	if err = s.ensureRuntimeMCPForSession(ctx, request.SessionID, candidate); err != nil {
		return nil, err
	}
	pair := sessionInputSelection{BasePrompt: candidate.baseSystemPrompt, Prompt: candidate.systemPrompt, Tools: candidate.toolsSetting}
	if ticket.owner {
		result, refreshErr := refresher.RefreshSessionInputs(ctx, request.SessionID, pair.Prompt, pair.Tools, equalSessionTools)
		if refreshErr != nil {
			return nil, refreshErr
		}
		committed = result.Changed()
		if committed {
			ticket.previous = nil
		}
		pair.Tools = result.Tools
		if !request.fresh && !result.Changed() && old != nil && old.store != nil && old.systemPrompt == pair.Prompt && equalSessionTools(old.toolsSetting, pair.Tools) {
			old.toolsSetting = pair.Tools
			if old.sessionMeta != nil {
				old.sessionMeta.Tools = pair.Tools
			}
			old.inputs.Store(ticket.finish(pair))
			// The existing executable registry and prompt already match. Keep
			// its live provider continuation and context-estimate baseline.
			return old, nil
		}
	}
	// After commit complete publication even if the requester disconnected. No
	// continuation import or model execution occurs during this preparation.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	candidate.toolsSetting = pair.Tools
	if candidate.sessionMeta != nil {
		candidate.sessionMeta.Tools = pair.Tools
	}
	if !candidate.ensurePersistedSession(finishCtx, request.SessionID, nil) {
		return nil, errServeSessionPersistence
	}
	applyPersistedContextEstimate(candidate.engine, candidate.sessionMeta)
	if old != nil && !request.fresh && old.engine != nil {
		total, count := old.engine.ContextEstimateBaseline()
		if total > 0 {
			candidate.engine.SetContextEstimateBaseline(total, count)
		}
	}
	candidate.configureSideQuestionContext()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errServeSessionManagerClosed
	}
	if ticket.owner {
		candidate.inputs.Store(ticket.finish(pair))
	} else if ticket.current() {
		candidate.inputs.Store(request.Inputs)
	}
	if candidate.inputs.Load() == nil {
		m.mu.Unlock()
		return nil, errServeSessionBusy
	}
	candidate.Touch()
	m.sessions[request.SessionID] = candidate
	published = true
	if old != nil {
		old.retiredInputs.Store(true)
	}
	m.mu.Unlock()
	release()
	if old != nil {
		m.retireRuntime(old)
	}
	return candidate, nil
}

// admitSynchronousActivity transfers a short identity/installation pin to an
// activity lease before releasing the operation lock. Metadata writes remain
// available while lifecycle operations still see the runtime as busy, even
// before provider execution or between fallback/goal turns. A model swap also
// leases its retained source so rollback cannot expose a false idle window.
func (m *serveSessionManager) admitSynchronousActivity(id string, rt *serveRuntime, retained ...*serveRuntime) (func(), error) {
	releasePin, err := m.pinCurrentRuntime(id, rt)
	if err != nil {
		return nil, err
	}
	owners := []*serveRuntime{rt}
	for _, candidate := range retained {
		if candidate == nil {
			continue
		}
		duplicate := false
		for _, owner := range owners {
			duplicate = duplicate || candidate == owner
		}
		if !duplicate {
			owners = append(owners, candidate)
		}
	}
	for _, owner := range owners {
		owner.admittedActivity.Add(1)
	}
	releasePin()
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, owner := range owners {
				owner.admittedActivity.Add(-1)
			}
		})
	}, nil
}

// pinCurrentRuntime orders admission with replacement. Callers must release
// after publishing activity ownership, never hold it across a model call.
func (m *serveSessionManager) pinCurrentRuntime(id string, rt *serveRuntime) (func(), error) {
	operation := m.sessionOperation(id)
	if !operation.TryLock() {
		return nil, errServeSessionBusy
	}
	m.mu.Lock()
	current := m.sessions[id]
	_, reserved := m.reserved.Load(id)
	if m.closed || reserved || current != rt || m.creating[id] != nil {
		m.mu.Unlock()
		operation.Unlock()
		return nil, errServeSessionBusy
	}
	m.reserved.Store(id, struct{}{})
	m.mu.Unlock()
	return func() { m.reserved.Delete(id); operation.Unlock() }, nil
}

// Metadata writers need not wait for an active streaming runtime mutex, but
// their read/update pair must not straddle a committed input replacement.
func (s *serveServer) lockSessionInputMetadata(id string) (func(), error) {
	if s.sessionMgr == nil {
		return func() {}, nil
	}
	operation := s.sessionMgr.sessionOperation(id)
	if !operation.TryLock() {
		return nil, errServeSessionBusy
	}
	return operation.Unlock, nil
}
