package chat

import (
	"context"
	"errors"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

// claimStreamTurnLease takes the claim sendMessage made before its first write,
// or claims the turn for resumed and synthetic turns (reload resume, steering
// flush, direct shell). A displaced active claim is released, never leaked.
func (m *Model) claimStreamTurnLease(ctx context.Context, sessionID string) (*turnLease, error) {
	lease := m.pendingTurnLease.Swap(nil)
	if lease == nil {
		claimed, err := m.acquireTurnLease(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		lease = claimed
	}
	if lease != nil {
		m.releaseTurnLease(m.activeTurnLease.Swap(lease), session.ResponseRunCancelled)
	}
	return lease, nil
}

// finalizeMainRunTurn writes the terminal status while this process still owns
// the turn, then releases the lease; releasing first would let a successor's
// Active status be overwritten by this stale terminal write. A suspended run
// keeps its lease for suspendForReload, which still rewrites under its fence.
func (m *Model) finalizeMainRunTurn(lease *turnLease, sessionID string, runErr error) {
	status := session.StatusComplete
	var suspended *engine.SuspendedError
	if errors.As(runErr, &suspended) {
		status = session.StatusActive
	} else if runErr != nil {
		status = session.StatusError
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			status = session.StatusInterrupted
		}
	}
	if m.store != nil {
		_ = m.store.UpdateStatus(context.Background(), sessionID, status)
	}
	if suspended == nil {
		m.releaseTurnLease(lease, turnLeaseOutcome(runErr))
	}
}
