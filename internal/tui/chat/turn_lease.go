package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

// One durable turn owner per process. Every session this process runs a turn in
// is claimed under the same identity, so in-process serialization (the main run
// manager) stays authoritative and only other processes are locked out.
var tuiTurnOwnerID = sync.OnceValue(func() string { return "tui_" + session.NewID() })

// errTranscriptStale reports that the durable transcript moved while this
// process was not looking, so its in-memory history is no longer a safe
// continuation.
var errTranscriptStale = errors.New("chat: session transcript changed in another process")

const (
	turnLeaseDuration       = 30 * time.Second
	turnLeaseRenewInterval  = 10 * time.Second
	turnLeaseRenewTimeout   = 5 * time.Second
	turnLeaseMinRemaining   = 12 * time.Second
	turnLeaseAdmitTimeout   = 5 * time.Second
	turnLeaseReleaseTimeout = 4 * time.Second
)

// turnLease is this process's durable claim on one session's turn. It fences
// every transcript write made under its context, so a lease lost to another
// process cannot commit assistant or tool rows.
type turnLease struct {
	store     session.ServeResponseLifecycleStore
	attention session.AttentionStore
	indexer   session.TranscriptIndexer
	noteRev   func(sessionID string, rev int64)
	fence     session.ResponseRunFence
	sessionID string
	expiresAt atomic.Int64
	cancelRun atomic.Pointer[func()]
	stop      chan struct{}
	released  sync.Once
}

// acquireTurnLease claims the session's turn before the first turn-associated
// write. It returns a nil lease with no error when the store cannot own leases
// (no session storage, read-only, or a session that was never persisted), so
// those setups keep their previous behaviour.
func (m *Model) acquireTurnLease(ctx context.Context, sessionID string) (*turnLease, error) {
	if m == nil || m.store == nil || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	lifecycle, ok := session.AsServeResponseLifecycleStore(m.store)
	if !ok {
		return nil, nil
	}
	admitCtx, cancel := context.WithTimeout(ctx, turnLeaseAdmitTimeout)
	defer cancel()
	owner := tuiTurnOwnerID()
	responseID := "tui_turn_" + session.NewID()
	lease, err := lifecycle.AdmitResponseRun(admitCtx, session.ResponseRunAdmission{
		ResponseID:      responseID,
		SessionID:       sessionID,
		RunEpoch:        time.Now().UnixNano(),
		OwnerInstanceID: owner,
		StartedRev:      m.knownTranscriptRev.Load(),
		StartedAt:       time.Now(),
		LeaseDuration:   turnLeaseDuration,
	})
	if errors.Is(err, session.ErrNotFound) {
		// The session row does not exist, so no other process can be sharing it.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim session turn: %w", err)
	}
	turn := &turnLease{
		store:     lifecycle,
		noteRev:   m.noteOwnRunRev,
		sessionID: sessionID,
		stop:      make(chan struct{}),
		fence: session.ResponseRunFence{
			ResponseID:      responseID,
			OwnerInstanceID: owner,
			FencingToken:    lease.FencingToken,
		},
	}
	turn.indexer, _ = m.store.(session.TranscriptIndexer)
	turn.attention, _ = session.AsAttentionStore(m.store)
	turn.expiresAt.Store(lease.LeaseExpiresAt.UnixMilli())
	// The provider request is built from history this process loaded. Refuse to
	// continue a transcript another process changed rather than silently sending
	// a stale conversation. A revision this process produced itself (skill
	// activation rows, undo, shell turns, ...) is not another process's change.
	if rev, ok := turn.transcriptRev(admitCtx); ok {
		if !m.transcriptRevObserved.Load() {
			m.knownTranscriptRev.Store(rev)
			m.transcriptRevObserved.Store(true)
		} else if rev != m.knownTranscriptRev.Load() && !m.ownTranscriptRev(rev, sessionID) {
			turn.release(session.ResponseRunCancelled)
			return nil, errTranscriptStale
		}
	}
	return turn, nil
}

// ownTranscriptRev reports whether rev is the last revision this process's
// store produced for the session.
func (m *Model) ownTranscriptRev(rev int64, sessionID string) bool {
	reporter, ok := m.store.(session.OwnTranscriptRevReporter)
	if !ok {
		return false
	}
	own, ok := reporter.OwnTranscriptRev(sessionID)
	return ok && own == rev
}

// context carries the lease fence into every transcript write of the run.
func (l *turnLease) context(ctx context.Context) context.Context {
	if l == nil {
		return ctx
	}
	return session.WithResponseRunFence(ctx, l.fence)
}

// startRenewals keeps the durable lease alive for as long as the run does, and
// fails closed by cancelling the run when ownership is lost or unverifiable.
func (l *turnLease) startRenewals(runCtx context.Context, cancelRun func()) {
	if l == nil {
		return
	}
	if cancelRun != nil {
		l.cancelRun.Store(&cancelRun)
	}
	go func() {
		ticker := time.NewTicker(turnLeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			renewCtx, cancel := context.WithTimeout(context.Background(), turnLeaseRenewTimeout)
			renewed, err := l.store.RenewResponseRunLease(renewCtx, l.fence.ResponseID, l.fence.OwnerInstanceID, l.fence.FencingToken)
			cancel()
			if err == nil {
				l.expiresAt.Store(renewed.LeaseExpiresAt.UnixMilli())
				continue
			}
			lost := errors.Is(err, session.ErrResponseRunLeaseLost)
			if lost || time.Until(time.UnixMilli(l.expiresAt.Load())) < turnLeaseMinRemaining {
				if cancelRun != nil {
					cancelRun()
				}
				return
			}
		}
	}()
}

// lost stops the run once a fenced write reports that ownership is gone, so a
// fenced-out process does not keep streaming output it can no longer persist.
func (l *turnLease) lost() {
	if l == nil {
		return
	}
	if cancelRun := l.cancelRun.Load(); cancelRun != nil {
		(*cancelRun)()
	}
}

// release stops renewals and finalizes the durable lease so another process may
// own the next turn. It is best effort: an unreachable store leaves a lease that
// expires on its own.
func (l *turnLease) release(outcome session.ResponseRunState) {
	if l == nil {
		return
	}
	l.released.Do(func() {
		close(l.stop)
		ctx, cancel := context.WithTimeout(context.Background(), turnLeaseReleaseTimeout)
		defer cancel()
		rev, ok := l.transcriptRev(ctx)
		if ok && l.noteRev != nil {
			// This process owned the turn, so every revision up to here is its own.
			l.noteRev(l.sessionID, rev)
		}
		attention, err := l.store.FinalizeResponseRun(ctx, session.ResponseRunTerminal{
			ResponseID:      l.fence.ResponseID,
			OwnerInstanceID: l.fence.OwnerInstanceID,
			FencingToken:    l.fence.FencingToken,
			Outcome:         outcome,
			FinalRev:        rev,
			EndedAt:         time.Now(),
		})
		// The terminal marker exists for surfaces that were not watching. This
		// process showed the outcome as it happened, so do not leave an unseen
		// badge behind in the web sidebar or hub for every terminal turn.
		if err == nil && attention.Changed && l.attention != nil {
			_, _ = l.attention.MarkAttentionSeen(ctx, l.sessionID, attention.StoreInstanceID, attention.LatestAttentionSeq)
		}
	})
}

func (l *turnLease) transcriptRev(ctx context.Context) (int64, bool) {
	if l == nil {
		return 0, false
	}
	return transcriptRevOf(ctx, l.indexer, l.sessionID)
}

// transcriptRevOf reads the durable revision when the store tracks one.
func transcriptRevOf(ctx context.Context, indexer session.TranscriptIndexer, sessionID string) (int64, bool) {
	if indexer == nil {
		return 0, false
	}
	rev, err := indexer.TranscriptRev(ctx, sessionID)
	if err != nil {
		return 0, false
	}
	return rev, true
}

// releaseTurnLease finalizes a lease and clears it from the model, so a turn
// that never started does not block the next one. Clearing is conditional: a
// newer turn's claim must not be dropped by an older turn settling late.
func (m *Model) releaseTurnLease(lease *turnLease, outcome session.ResponseRunState) {
	if lease == nil {
		return
	}
	m.pendingTurnLease.CompareAndSwap(lease, nil)
	m.activeTurnLease.CompareAndSwap(lease, nil)
	lease.release(outcome)
}

func turnLeaseOutcome(runErr error) session.ResponseRunState {
	switch {
	case runErr == nil:
		return session.ResponseRunCompleted
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		return session.ResponseRunCancelled
	default:
		return session.ResponseRunFailed
	}
}

// turnLeaseRefusal explains why this process may not run a turn right now.
func turnLeaseRefusal(err error) string {
	switch {
	case errors.Is(err, session.ErrSessionTurnOwned):
		return "Session is busy in another process (web); wait for that turn to finish."
	case errors.Is(err, errTranscriptStale):
		return "Session was changed by another process (web). Use /resume to reload before sending."
	default:
		return fmt.Sprintf("Could not claim this session's turn: %v", err)
	}
}

// noteOwnRunRev records a finished run's revision, but only while the model
// still shows that session: a run finishing in the background after a session
// switch must not poison the current session's synchronization point.
func (m *Model) noteOwnRunRev(sessionID string, rev int64) {
	if m == nil || sessionID == "" || m.SessionID() != sessionID {
		return
	}
	m.knownTranscriptRev.Store(rev)
	m.transcriptRevObserved.Store(true)
}

// handleFencedWriteError cancels the run when a transcript write was rejected
// because this process no longer owns the session's turn.
func (m *Model) handleFencedWriteError(err error) {
	if err == nil || !errors.Is(err, session.ErrResponseRunLeaseLost) {
		return
	}
	m.activeTurnLease.Load().lost()
}

// noteTranscriptRev records the revision this process is synchronized with,
// so only another process's writes are treated as staleness.
func (m *Model) noteTranscriptRev(ctx context.Context) {
	if m == nil || m.store == nil || m.sess == nil {
		return
	}
	indexer, _ := m.store.(session.TranscriptIndexer)
	if rev, ok := transcriptRevOf(ctx, indexer, m.sess.ID); ok {
		m.knownTranscriptRev.Store(rev)
		m.transcriptRevObserved.Store(true)
	}
}
