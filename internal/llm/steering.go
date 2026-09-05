package llm

import (
	"context"
	"errors"
)

const SteeringInterruptionNotice = "The previous run was interrupted to deliver pending steering. Partial actions may have completed and independent background processes may still be running. Check state before repeating side effects."

var ErrSteeringTransition = errors.New("steering transition in progress")

// SteeringTransition is an exclusive, generation-local ownership token. A
// snapshot remains frozen until rollback or explicit replacement handoff.
type SteeringTransition struct {
	OperationID string
	Fence       int64
}

type SteeringAvailability struct {
	Protocol          int    `json:"protocol"`
	CanSteer          bool   `json:"can_steer"`
	CanRush           bool   `json:"can_rush"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// Rush uses ordinary cancellation followed by the next conversation request.
// Provider-specific resume IDs and history replay stay owned by each adapter.
func (e *Engine) steeringAvailabilityLocked() SteeringAvailability {
	a := SteeringAvailability{Protocol: 1, CanSteer: e.steeringRunState == steeringRunAccepting}
	switch {
	case e.steeringTransition != nil:
		a.CanSteer = false
		a.UnavailableReason = "transition_in_progress"
	case !a.CanSteer:
		a.UnavailableReason = "run_not_consuming"
	default:
		a.CanRush = true
	}
	return a
}
func (e *Engine) SteeringAvailability() SteeringAvailability {
	e.callbackMu.RLock()
	defer e.callbackMu.RUnlock()
	return e.steeringAvailabilityLocked()
}
func (e *Engine) FreezeSteering(owner SteeringTransition) ([]QueuedSteering, error) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if owner.OperationID == "" || owner.Fence <= 0 {
		return nil, ErrSteeringTransition
	}
	a := e.steeringAvailabilityLocked()
	if !a.CanRush {
		return nil, errors.New(a.UnavailableReason)
	}
	entries := append([]QueuedSteering(nil), e.pendingSteering...)
	eligible := false
	for _, entry := range entries {
		eligible = eligible || entry.Origin.EligibleForRush()
	}
	if !eligible {
		return nil, nil
	}
	e.steeringTransition = &owner
	e.steeringTransitionDone = make(chan struct{})
	return entries, nil
}

// ReleaseSteeringFreeze rolls admission back without moving input or changing
// identity. On successful handoff, consume removes only this owner's snapshot.
func (e *Engine) ReleaseSteeringFreeze(owner SteeringTransition, consume bool) bool {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition == nil || *e.steeringTransition != owner {
		return false
	}
	if consume {
		for _, entry := range e.pendingSteering {
			e.rememberCommittedSteeringIDLocked(entry.ID)
		}
		e.pendingSteering = nil
	}
	e.steeringTransition = nil
	close(e.steeringTransitionDone)
	e.steeringTransitionDone = nil
	return true
}

// Dispatch waits on admission rather than manufacturing a cancellation before
// durable intent exists. Rollback wakes the source with its original queue.
func (e *Engine) awaitSteeringDispatch(ctx context.Context) error {
	for {
		e.callbackMu.RLock()
		done := e.steeringTransitionDone
		e.callbackMu.RUnlock()
		if done == nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}
}

// beginSteeringTool and actualSteeringToolDone track actual Execute lifetime,
// not the synthetic cancellation result seen by the run. No unsafe overlap is
// permitted even if the source stream has already returned.
func (e *Engine) beginSteeringTool(ctx context.Context) error {
	for {
		if err := e.awaitSteeringDispatch(ctx); err != nil {
			return err
		}
		e.callbackMu.Lock()
		if e.steeringTransition != nil {
			e.callbackMu.Unlock()
			continue
		}
		if e.activeSteeringTools == 0 {
			e.steeringToolsSettled = make(chan struct{})
		}
		e.activeSteeringTools++
		e.callbackMu.Unlock()
		return nil
	}
}
func (e *Engine) actualSteeringToolDone() {
	e.callbackMu.Lock()
	e.activeSteeringTools--
	if e.activeSteeringTools == 0 {
		close(e.steeringToolsSettled)
		e.steeringToolsSettled = nil
	}
	e.callbackMu.Unlock()
}

// WaitSteeringSettlement is independent of synthetic tool results. The freeze
// remains installed until actual execution has drained; timeout never opens it.
func (e *Engine) WaitSteeringSettlement(ctx context.Context, owner SteeringTransition) error {
	e.callbackMu.RLock()
	if e.steeringTransition == nil || *e.steeringTransition != owner {
		e.callbackMu.RUnlock()
		return ErrSteeringTransition
	}
	done := e.steeringToolsSettled
	e.callbackMu.RUnlock()
	if done == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (e *Engine) SteeringTransitioning() bool {
	e.callbackMu.RLock()
	defer e.callbackMu.RUnlock()
	return e.steeringTransition != nil
}

// Producers derive review provenance from typed review parts, never ID prefixes.
func SteeringOriginForMessage(message Message) SteeringOrigin {
	for _, part := range message.Parts {
		if part.Type == PartDiffComment {
			return SteeringOriginReview
		}
	}
	return SteeringOriginUser
}
