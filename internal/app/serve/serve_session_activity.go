package serve

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
)

// serveSessionActivity is a process-local lifecycle snapshot, independent of a
// response run. Terminal results remain available to every observer until the
// next attempt or runtime eviction; they are not durable cross-process outcomes.
// Status is running, completed, failed, or cancelled. Cancellation/suspension
// ends an attempt without reporting a user-visible compaction failure.
type serveSessionActivity struct {
	ID            string `json:"id"`
	Revision      int64  `json:"revision"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	StartedAt     int64  `json:"started_at"`
	Error         string `json:"error,omitempty"`
	Message       string `json:"message,omitempty"` // user-facing completion note
	TranscriptRev int64  `json:"transcript_rev,omitempty"`
}

type runtimeSessionActivity struct {
	mu                 sync.Mutex
	snapshot           *serveSessionActivity
	publish            func()
	transcriptRevision func(context.Context) (int64, error)
}

func (rt *serveRuntime) activitySnapshot() *serveSessionActivity {
	if rt == nil {
		return nil
	}
	rt.activity.mu.Lock()
	defer rt.activity.mu.Unlock()
	if rt.activity.snapshot == nil {
		return nil
	}
	copy := *rt.activity.snapshot
	return &copy
}

func (s *serveServer) bindRuntimeActivity(rt *serveRuntime, sessionID string) {
	if rt == nil || sessionID == "" {
		return
	}
	rt.activity.mu.Lock()
	rt.activity.transcriptRevision = func(ctx context.Context) (int64, error) {
		return s.transcriptRev(ctx, sessionID)
	}
	rt.activity.publish = func() {
		s.publishEvent(serveEventInput{Type: serveEventSessionRuntimeChanged, SessionID: sessionID, Reason: "compaction"})
	}
	rt.activity.mu.Unlock()
}

func (rt *serveRuntime) beginCompactionActivity() {
	rt.activity.mu.Lock()
	revision := int64(1)
	if previous := rt.activity.snapshot; previous != nil {
		revision = previous.Revision + 1
	}
	rt.activity.snapshot = &serveSessionActivity{
		ID: "compaction_" + randomSuffix(), Revision: revision, Kind: "compaction",
		Status: "running", StartedAt: time.Now().UnixMilli(),
	}
	publish := rt.activity.publish
	rt.activity.mu.Unlock()
	if publish != nil {
		publish()
	}
}

// noteCompactionActivity attaches a completion note to the running attempt;
// it is published with the terminal snapshot.
func (rt *serveRuntime) noteCompactionActivity(message string) {
	rt.activity.mu.Lock()
	defer rt.activity.mu.Unlock()
	if current := rt.activity.snapshot; current != nil && current.Status == "running" {
		current.Message = message
	}
}

func (rt *serveRuntime) finishCompactionActivity(err error) {
	rt.finishCompactionActivityWithRelease(err, nil)
}

func (rt *serveRuntime) finishCompactionActivityWithRelease(err error, release func()) {
	rt.activity.mu.Lock()
	current := rt.activity.snapshot
	running := current != nil && current.Status == "running"
	readRevision := rt.activity.transcriptRevision
	rt.activity.mu.Unlock()
	// Callers own rt.mu until terminal publication (manual release below or
	// the engine stream consumer). Never hold the snapshot lock over DB work.
	var transcriptRev int64
	if err == nil && running && readRevision != nil {
		ctx, cancel := context.WithTimeout(context.Background(), responseRunRevisionReadTimeout)
		if revision, readErr := readRevision(ctx); readErr == nil {
			transcriptRev = revision
		}
		cancel()
	}
	rt.activity.mu.Lock()
	if release != nil {
		release()
	}
	if rt.activity.snapshot == nil || rt.activity.snapshot != current || current.Status != "running" {
		rt.activity.mu.Unlock()
		return
	}
	current.Revision++
	current.Status = "completed"
	current.TranscriptRev = transcriptRev
	var suspended *engine.SuspendedError
	switch {
	case errors.Is(err, context.Canceled), errors.As(err, &suspended):
		current.Status = "cancelled"
	case err != nil:
		current.Status = "failed"
		current.Error = err.Error()
	}
	publish := rt.activity.publish
	rt.activity.mu.Unlock()
	if publish != nil {
		publish()
	}
}

// recordRuntimeEvent is shared by every engine stream consumer, including
// non-Responses entrypoints. Human-readable phase text is not lifecycle truth.
func (rt *serveRuntime) recordRuntimeEvent(event core.Event) {
	rt.recordStatsEvent(event)
	if event.Type != core.EventCompactionStatus {
		return
	}
	if event.CompactionActive {
		rt.beginCompactionActivity()
	} else {
		rt.finishCompactionActivity(event.Err)
	}
}

func (rt *serveRuntime) finishRuntimeStream(ctx context.Context, err error) {
	rt.finishStats()
	// A transport consumer may stop before receiving the engine's terminal
	// status (for example on disconnect). Do not leave a phantom running slot.
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = errors.New("compaction stream ended before completion")
	}
	rt.finishCompactionActivity(err)
}

// tryLockRun rejects before input persistence, even in the reservation-to-worker
// gap when a manual compaction owns admission but has not begun provider work.
func (rt *serveRuntime) tryLockRun() bool {
	if rt.compacting.Load() || !rt.mu.TryLock() {
		return false
	}
	if rt.compacting.Load() {
		rt.mu.Unlock()
		return false
	}
	return true
}

func (s *serveServer) sessionActivitySnapshot(sessionID string) *serveSessionActivity {
	if s.sessionMgr != nil {
		return s.sessionMgr.activitySnapshot(sessionID)
	}
	return nil
}

// activitySnapshot observes cached work without extending the runtime's TTL.
func (m *serveSessionManager) activitySnapshot(id string) *serveSessionActivity {
	m.mu.Lock()
	rt := m.sessions[id]
	m.mu.Unlock()
	return rt.activitySnapshot()
}

func (m *serveSessionManager) activeCompactionIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, rt := range m.sessions {
		if activity := rt.activitySnapshot(); activity != nil && activity.Status == "running" {
			ids = append(ids, id)
		}
	}
	return ids
}
