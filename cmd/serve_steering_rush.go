package cmd

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type rushFailure struct {
	status session.RushStatus
	reason string
}

type rushContextKey struct{}
type rushInitialInputKey struct{}

func rushFromContext(ctx context.Context) *session.RushOperation {
	op, _ := ctx.Value(rushContextKey{}).(*session.RushOperation)
	return op
}

type serveSteeringTransition struct {
	mu              sync.Mutex // operation CAS + replacement admission; never held by engine callbacks
	op              *session.RushOperation
	replacementID   string
	source          *responseRun
	runtime         *serveRuntime
	owner           llm.SteeringTransition
	admitted        chan struct{}
	cancelRequested atomic.Bool
	finish          sync.Once
	waitOnce        sync.Once
	failure         atomic.Pointer[rushFailure]
}

func (m *responseRunManager) steeringTransition(sessionID string) *serveSteeringTransition {
	if m == nil {
		return nil
	}
	v, ok := m.steeringTransitions.Load(sessionID)
	if !ok {
		return nil
	}
	return v.(*serveSteeringTransition)
}

func (s *serveServer) handleSteeringRush(w http.ResponseWriter, r *http.Request, sessionID, suffix string) {
	store, ok := session.AsRushStore(s.store)
	if !ok {
		writeOpenAIError(w, 422, "rush_unavailable", "durable_store_unavailable")
		return
	}
	if suffix != "" {
		id := strings.TrimSuffix(suffix, "/cancel")

		if r.Method == http.MethodPost && suffix != id {
			op, err := s.cancelSteeringRush(r.Context(), store, sessionID, id)
			if err != nil {
				writeOpenAIError(w, 409, "rush_conflict", err.Error())
				return
			}
			writeJSON(w, 200, op)
			return
		}
		op, err := store.GetRush(r.Context(), sessionID, id)
		if err != nil {
			writeOpenAIError(w, 404, "not_found_error", "rush operation not found")
			return
		}
		if r.Method == http.MethodGet && suffix == id {
			writeJSON(w, 200, op)
			return
		}

		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var req struct {
		ExpectedResponseID string `json:"expected_response_id"`
		ExpectedRunEpoch   int64  `json:"expected_run_epoch"`
		RequestID          string `json:"request_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil || strings.TrimSpace(req.RequestID) == "" || strings.TrimSpace(req.ExpectedResponseID) == "" || req.ExpectedRunEpoch <= 0 {
		writeOpenAIError(w, 400, "invalid_request_error", "request_id, expected_response_id and positive expected_run_epoch are required")
		return
	}
	if existing, err := store.GetRush(r.Context(), sessionID, req.RequestID); err == nil {
		if existing.SourceResponseID != req.ExpectedResponseID || existing.SourceEpoch != req.ExpectedRunEpoch {
			writeOpenAIError(w, 409, "rush_conflict", "request ID already used for another source")
			return
		}
		writeJSON(w, 200, existing)
		return
	} else if !errors.Is(err, session.ErrNotFound) {
		writeOpenAIError(w, 500, "server_error", "failed to read rush operation")
		return
	}

	if transition := s.ensureResponseRuns().steeringTransition(sessionID); transition != nil && transition.owner.OperationID == req.RequestID {
		if transition.source.id != req.ExpectedResponseID || transition.source.runEpoch != req.ExpectedRunEpoch {
			writeOpenAIError(w, 409, "rush_conflict", "request ID already used for another source")
			return
		}
		select {
		case <-transition.admitted:
		case <-r.Context().Done():
			return
		}
		if op, err := store.GetRush(r.Context(), sessionID, req.RequestID); err == nil {
			writeJSON(w, 200, op)
		} else {
			writeOpenAIError(w, 500, "server_error", "rush admission did not commit")
		}
		return
	}
	if s.sessionMgr == nil {
		writeOpenAIError(w, 404, "not_found_error", "session not found")
		return
	}
	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok || rt.engine == nil {
		writeOpenAIError(w, 409, "response_owner_conflict", "no active run")
		return
	}
	mgr := s.ensureResponseRuns()
	boundary := mgr.sessionBoundary(sessionID)
	boundary.Lock()
	source, _ := mgr.get(req.ExpectedResponseID)
	if mgr.activeRunID(sessionID) != req.ExpectedResponseID || source == nil || source.runEpoch != req.ExpectedRunEpoch || mgr.steeringTransition(sessionID) != nil {
		boundary.Unlock()
		writeOpenAIError(w, 409, "response_owner_conflict", "active response changed or transitioning")
		return
	}
	if source.settled == nil || !source.rushStateful {
		boundary.Unlock()
		writeOpenAIError(w, 422, "rush_unavailable", "run_not_consuming")
		return
	}
	owner := llm.SteeringTransition{OperationID: req.RequestID, Fence: req.ExpectedRunEpoch}
	rt.steeringMutationMu.Lock()
	entries, err := rt.engine.FreezeSteering(owner)
	rt.steeringMutationMu.Unlock()
	if err != nil {
		boundary.Unlock()
		writeOpenAIError(w, 422, "rush_unavailable", err.Error())
		return
	}
	transition := &serveSteeringTransition{source: source, runtime: rt, owner: owner, replacementID: "resp_" + randomSuffix(), admitted: make(chan struct{})}
	mgr.steeringTransitions.Store(sessionID, transition)
	boundary.Unlock()
	pending := make([]session.PendingSteering, 0, len(entries))
	for _, entry := range entries {
		pending = append(pending, session.PendingSteering{SessionID: sessionID, ID: entry.ID, Message: entry.Message, DisplayText: entry.DisplayText, Origin: entry.Origin})
	}
	admissionCtx, admissionCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 4*time.Second)
	defer admissionCancel()
	op, err := store.AdmitRush(admissionCtx, session.RushOperation{SessionID: sessionID, RequestID: req.RequestID, SourceResponseID: source.id, SourceEpoch: source.runEpoch, Fence: owner.Fence, ReplacementResponseID: transition.replacementID}, pending)
	if err != nil {
		close(transition.admitted)
		rt.engine.ReleaseSteeringFreeze(owner, false)
		mgr.steeringTransitions.Delete(sessionID)
		writeOpenAIError(w, 500, "server_error", "failed to persist rush; current run was not interrupted")
		return
	}
	transition.op = op
	close(transition.admitted)
	if op.Status == session.RushNoop {
		mgr.steeringTransitions.Delete(sessionID)
		writeJSON(w, 200, op)
		return
	}
	// Admission belongs to the server, never to the HTTP connection.
	go s.coordinateSteeringRush(store, transition)
	writeJSON(w, http.StatusAccepted, op)
}

func (s *serveServer) coordinateSteeringRush(store session.RushStore, t *serveSteeringTransition) {
	op := t.op
	sid := op.SessionID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	block := func(reason string) {
		// Keep the reservation and freeze until the captured source actually
		// settles. A timeout is not permission to overlap abandoned work.
		t.source.cancelRun()
		s.finishSteeringRush(store, t, session.RushBlocked, reason)
	}
	if t.cancelRequested.Load() {
		block("stopped by user")
		return
	}

	// Pause before cancellation, so terminal callbacks cannot schedule a goal pass.
	sess, err := s.store.Get(ctx, sid)
	if err != nil {
		block("failed to read goal state")
		return
	}
	if sess.Goal != nil && sess.Goal.IsActive() {
		goal := sess.Goal.Clone()
		goal.Status = session.GoalStatusPaused
		goal.PausedAt = time.Now()
		goal.UpdatedAt = goal.PausedAt
		goal.LastReason = "paused for steering rush"
		if err = session.UpdateGoal(ctx, s.store, sid, goal); err != nil {
			block("failed to pause goal")
			return
		}
	}
	t.source.cancelRun()
	if current, err := store.GetRush(ctx, sid, op.RequestID); err == nil && current.Status == session.RushInterrupting {
		if next, err := store.AdvanceRush(ctx, current, session.RushWaiting, ""); err == nil {
			op = next
		}
	}
	select {
	case <-t.source.settled:
	case <-ctx.Done():
		block("settlement_unknown")
		return
	}
	t.source.mu.Lock()
	persistenceFailed := t.source.durableHandoffErr != ""
	t.source.mu.Unlock()
	if persistenceFailed {
		block("source persistence did not settle")
		return
	}
	op, err = store.GetRush(ctx, sid, op.RequestID)
	if err != nil {
		block("operation state unavailable")
		return
	}
	if !op.Status.Active() {
		s.finishSteeringRush(store, t, op.Status, op.Reason)
		return
	}
	if err = t.runtime.engine.WaitSteeringSettlement(ctx, t.owner); err != nil {
		block("settlement_unknown")
		return
	}
	op, err = store.AdvanceRush(ctx, op, session.RushStarting, "")
	if err != nil {
		block("replacement authorization changed")
		return
	}
	// Frozen entries cannot have been drained by the source. Retain each row and
	// client ID as an ordered batch, not a text concatenation or fake user notice.
	messages := []llm.Message{{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: llm.SteeringInterruptionNotice}}}}
	for _, entry := range op.Entries {
		messages = append(messages, entry.Steering.Message)
	}
	req := t.source.rushRequest
	_, err = s.startResponseRun(t.runtime, true, false, messages, req, sid, startResponseRunOptions{uiSession: true, previousResponseID: t.source.id, rush: op,
		onInitialInput: func() {
			t.finish.Do(func() {
				t.runtime.engine.ReleaseSteeringFreeze(t.owner, true)
				s.responseRuns.steeringTransitions.CompareAndDelete(sid, t)
			})
		},
		onDone: func() {
			s.finishSteeringRush(store, t, session.RushFailed, "replacement stopped before initial input committed")
		},
	})
	if err != nil {
		block("replacement admission failed")
	}
}

func (s *serveServer) cancelSteeringRush(ctx context.Context, store session.RushStore, sessionID, id string) (*session.RushOperation, error) {
	if t := s.ensureResponseRuns().steeringTransition(sessionID); t != nil && t.owner.OperationID == id {
		t.cancelRequested.Store(true)
		t.source.cancelRun()
		select {
		case <-t.admitted:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	op, err := store.GetRush(ctx, sessionID, id)
	if err != nil {
		return nil, err
	}
	if op.Status.Active() {
		op, err = store.AdvanceRush(ctx, op, session.RushCancelled, "stopped by user")
		if errors.Is(err, session.ErrSteeringConflict) {
			op, err = store.GetRush(ctx, sessionID, id)
		}
	}
	if err == nil && op.Status == session.RushStarted {
		if run, ok := s.ensureResponseRuns().get(op.ReplacementResponseID); ok {
			run.cancelRun()
		}
	}
	return op, err
}

// Failed operations remain recoverable. Only a verified source settlement may
// release the freeze/ownership. An uncooperative source keeps the reservation
// visibly blocked until it exits rather than allowing overlapping side effects.
func (s *serveServer) finishSteeringRush(store session.RushStore, t *serveSteeringTransition, status session.RushStatus, reason string) {
	t.failure.CompareAndSwap(nil, &rushFailure{status: status, reason: reason})
	if !t.mu.TryLock() {
		return
	}
	defer t.mu.Unlock()
	if s.ensureResponseRuns().steeringTransition(t.op.SessionID) != t {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op, err := store.GetRush(ctx, t.op.SessionID, t.op.RequestID)
	if err != nil {
		return
	} // a later state reconciliation retries durable cleanup
	if op.Status == session.RushStarted {
		return
	}
	if op.Status.Active() {
		op, err = store.AdvanceRush(ctx, op, status, reason)
		if err != nil {
			return
		}
	}
	select {
	case <-t.source.settled:
		if err := t.runtime.engine.WaitSteeringSettlement(ctx, t.owner); err != nil {
			return
		}
		if err := store.ReleaseRush(ctx, op); err != nil {
			return
		}
		t.finish.Do(func() {
			t.runtime.engine.ReleaseSteeringFreeze(t.owner, false)
			s.responseRuns.steeringTransitions.CompareAndDelete(op.SessionID, t)
		})
	default:
		t.waitOnce.Do(func() {
			go func() {
				select {
				case <-t.source.settled:
					s.finishSteeringRush(store, t, status, reason)
				case <-s.shutdownCh:
				}
			}()
		})
	}
}
