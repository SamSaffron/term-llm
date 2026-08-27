package cmd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const defaultServeSessionRetirementTimeout = 2 * time.Second

var errServeSessionManagerClosed = errors.New("session manager closed")

type serveSessionManager struct {
	ttl               time.Duration
	max               int
	factory           func(context.Context) (*serveRuntime, error)
	onEvict           func(rt *serveRuntime) // called when a session is evicted
	retirementTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*serveRuntime
	creating map[string]*sessionCreateInFlight
	closed   bool
	stopCh   chan struct{}

	// Per-session operation locks keep runtime installation/replacement and
	// metadata mutations ordered without holding the process-wide map mutex.
	operations sync.Map // map[session ID]*sync.Mutex
	reserved   sync.Map // session IDs pinned against eviction during metadata I/O
}

type sessionCreateInFlight struct {
	done chan struct{}
	rt   *serveRuntime
	err  error
}

func (m *serveSessionManager) sessionOperation(id string) *sync.Mutex {
	operation, _ := m.operations.LoadOrStore(id, &sync.Mutex{})
	return operation.(*sync.Mutex)
}

func (m *serveSessionManager) lockSessionOperation(id string) func() {
	operation := m.sessionOperation(id)
	operation.Lock()
	return operation.Unlock
}

// lockIdleMetadataMutation pins one session's runtime identity while metadata
// I/O runs. It never holds the process-wide session map mutex across that I/O.
func (m *serveSessionManager) lockIdleMetadataMutation(id string) (*serveRuntime, func(), error) {
	unlockOperation := m.lockSessionOperation(id)
	m.reserved.Store(id, struct{}{})

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.reserved.Delete(id)
		unlockOperation()
		return nil, nil, errServeSessionManagerClosed
	}
	if m.creating[id] != nil {
		m.mu.Unlock()
		m.reserved.Delete(id)
		unlockOperation()
		return nil, nil, errServeSessionBusy
	}
	rt := m.sessions[id]
	if rt != nil && (rt.hasActiveRun() || !rt.mu.TryLock()) {
		m.mu.Unlock()
		m.reserved.Delete(id)
		unlockOperation()
		return nil, nil, errServeSessionBusy
	}
	m.mu.Unlock()

	var once sync.Once
	return rt, func() {
		once.Do(func() {
			if rt != nil {
				rt.mu.Unlock()
			}
			m.reserved.Delete(id)
			unlockOperation()
		})
	}, nil
}

func newServeSessionManager(ttl time.Duration, max int, factory func(context.Context) (*serveRuntime, error)) *serveSessionManager {
	m := &serveSessionManager{
		ttl:               ttl,
		max:               max,
		factory:           factory,
		retirementTimeout: defaultServeSessionRetirementTimeout,
		sessions:          make(map[string]*serveRuntime),
		creating:          make(map[string]*sessionCreateInFlight),
		stopCh:            make(chan struct{}),
	}
	go m.janitor()
	return m
}

func (m *serveSessionManager) janitor() {
	ticker := time.NewTicker(max(30*time.Second, m.ttl/2))
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.evictExpired()
		case <-m.stopCh:
			return
		}
	}
}

func (m *serveSessionManager) evictExpired() {
	now := time.Now()
	var stale []*serveRuntime

	m.mu.Lock()
	for id, rt := range m.sessions {
		if _, reserved := m.reserved.Load(id); reserved {
			continue
		}
		if now.Sub(rt.LastUsed()) > m.ttl && !rt.hasActiveActivity() {
			delete(m.sessions, id)
			stale = append(stale, rt)
		}
	}
	// Active sessions may temporarily overflow the cache target. As soon as
	// runtimes become idle, janitor passes trim the excess without affecting
	// admission or active work.
	for len(m.sessions) > m.max {
		evicted := m.evictOldestIdleLocked()
		if evicted == nil {
			break
		}
		stale = append(stale, evicted)
	}
	m.mu.Unlock()

	for _, rt := range stale {
		m.retireRuntime(rt)
	}
}

func (m *serveSessionManager) closeRuntime(rt *serveRuntime) {
	if rt == nil {
		return
	}
	timeout := m.retirementTimeout
	if timeout <= 0 {
		timeout = defaultServeSessionRetirementTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rt.CloseContext(ctx)
}

func (m *serveSessionManager) retireRuntime(rt *serveRuntime) {
	if rt == nil {
		return
	}
	if m.onEvict != nil {
		m.onEvict(rt)
	}
	m.closeRuntime(rt)
}

func (m *serveSessionManager) evictOldestIdleLocked() *serveRuntime {
	if len(m.sessions) < m.max {
		return nil
	}

	oldestID := ""
	var oldestTime time.Time
	for sid, srt := range m.sessions {
		if _, reserved := m.reserved.Load(sid); reserved {
			continue
		}
		if srt.hasActiveActivity() {
			continue
		}
		t := srt.LastUsed()
		if oldestID == "" || t.Before(oldestTime) {
			oldestID = sid
			oldestTime = t
		}
	}
	if oldestID == "" {
		return nil
	}

	evicted := m.sessions[oldestID]
	delete(m.sessions, oldestID)
	return evicted
}

func (m *serveSessionManager) makeRoomForNewSessionLocked() (*serveRuntime, error) {
	if len(m.sessions) < m.max {
		return nil, nil
	}

	if evicted := m.evictOldestIdleLocked(); evicted != nil {
		return evicted, nil
	}

	// max is an idle-runtime cache target, not an admission limit. If every
	// cached runtime is active, temporarily exceed it rather than allowing one
	// session's lifecycle to reject an unrelated session. Normal TTL/capacity
	// cleanup brings the cache back under the target once a runtime becomes idle.
	return nil, nil
}

// Get returns an existing session runtime without creating one.
// Returns (nil, false) if the session does not exist.
func (m *serveSessionManager) Get(id string) (*serveRuntime, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.sessions[id]
	if ok {
		rt.Touch()
	}
	return rt, ok
}

func (m *serveSessionManager) GetOrCreate(ctx context.Context, id string) (*serveRuntime, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errServeSessionManagerClosed
	}
	if rt, ok := m.sessions[id]; ok {
		rt.Touch()
		m.mu.Unlock()
		return rt, nil
	}
	if _, reserved := m.reserved.Load(id); reserved {
		m.mu.Unlock()
		return nil, errServeSessionBusy
	}
	if inflight, ok := m.creating[id]; ok {
		m.mu.Unlock()
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if inflight.err != nil {
			return nil, inflight.err
		}
		if inflight.rt == nil {
			return nil, fmt.Errorf("failed to initialize session runtime")
		}
		inflight.rt.Touch()
		return inflight.rt, nil
	}
	inflight := &sessionCreateInFlight{done: make(chan struct{})}
	m.creating[id] = inflight
	m.mu.Unlock()

	rt, err := m.factory(ctx)
	m.mu.Lock()
	delete(m.creating, id)

	var duplicate *serveRuntime
	var evicted *serveRuntime
	switch {
	case err != nil:
		inflight.err = err
	case m.closed:
		inflight.err = errServeSessionManagerClosed
	default:
		if existing, ok := m.sessions[id]; ok {
			existing.Touch()
			inflight.rt = existing
			duplicate = rt
		} else {
			rt.Touch()
			evicted, inflight.err = m.makeRoomForNewSessionLocked()
			if inflight.err == nil {
				m.sessions[id] = rt
				inflight.rt = rt
			}
		}
	}
	close(inflight.done)
	m.mu.Unlock()

	if duplicate != nil {
		m.closeRuntime(duplicate)
	}
	m.retireRuntime(evicted)
	if inflight.err != nil {
		if rt != nil && inflight.rt == nil {
			m.closeRuntime(rt)
		}
		return nil, inflight.err
	}
	if inflight.rt == nil {
		return nil, fmt.Errorf("failed to initialize session runtime")
	}
	inflight.rt.Touch()
	return inflight.rt, nil
}

// GetOrCreateWith is like GetOrCreate but uses a custom factory function.
// It shares the same in-flight deduplication so concurrent requests for the same
// session ID don't create multiple runtimes.
func (m *serveSessionManager) GetOrCreateWith(ctx context.Context, id string, create func(context.Context) (*serveRuntime, error)) (*serveRuntime, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errServeSessionManagerClosed
	}
	if rt, ok := m.sessions[id]; ok {
		rt.Touch()
		m.mu.Unlock()
		return rt, nil
	}
	if _, reserved := m.reserved.Load(id); reserved {
		m.mu.Unlock()
		return nil, errServeSessionBusy
	}
	if inflight, ok := m.creating[id]; ok {
		m.mu.Unlock()
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if inflight.err != nil {
			return nil, inflight.err
		}
		if inflight.rt == nil {
			return nil, fmt.Errorf("failed to initialize session runtime")
		}
		inflight.rt.Touch()
		return inflight.rt, nil
	}
	inflight := &sessionCreateInFlight{done: make(chan struct{})}
	m.creating[id] = inflight
	m.mu.Unlock()

	rt, err := create(ctx)
	m.mu.Lock()
	delete(m.creating, id)

	var duplicate *serveRuntime
	var evicted *serveRuntime
	switch {
	case err != nil:
		inflight.err = err
	case m.closed:
		inflight.err = errServeSessionManagerClosed
	default:
		if existing, ok := m.sessions[id]; ok {
			existing.Touch()
			inflight.rt = existing
			duplicate = rt
		} else {
			rt.Touch()
			evicted, inflight.err = m.makeRoomForNewSessionLocked()
			if inflight.err == nil {
				m.sessions[id] = rt
				inflight.rt = rt
			}
		}
	}
	close(inflight.done)
	m.mu.Unlock()

	if duplicate != nil {
		m.closeRuntime(duplicate)
	}
	m.retireRuntime(evicted)
	if inflight.err != nil {
		if rt != nil && inflight.rt == nil {
			m.closeRuntime(rt)
		}
		return nil, inflight.err
	}
	if inflight.rt == nil {
		return nil, fmt.Errorf("failed to initialize session runtime")
	}
	inflight.rt.Touch()
	return inflight.rt, nil
}

// ReplaceIdleWith atomically replaces a session when shouldReplace returns true.
// If the existing session does not need replacing, it is returned directly.
// This avoids the TOCTOU race between deleting and re-creating a session.
func (m *serveSessionManager) ReplaceIdleWith(ctx context.Context, id string, shouldReplace func(*serveRuntime) bool, create func(context.Context) (*serveRuntime, error)) (*serveRuntime, error) {
	m.mu.Lock()
	if _, reserved := m.reserved.Load(id); reserved {
		m.mu.Unlock()
		return nil, errServeSessionBusy
	}
	if m.closed {
		m.mu.Unlock()
		return nil, errServeSessionManagerClosed
	}

	var replaced *serveRuntime

	if rt, ok := m.sessions[id]; ok {
		if !shouldReplace(rt) {
			rt.Touch()
			m.mu.Unlock()
			return rt, nil
		}
		if rt.hasActiveActivity() {
			m.mu.Unlock()
			return nil, errServeSessionBusy
		}
		delete(m.sessions, id)
		replaced = rt
	}

	if inflight, ok := m.creating[id]; ok {
		m.mu.Unlock()
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if inflight.err != nil {
			return nil, inflight.err
		}
		if inflight.rt == nil {
			return nil, fmt.Errorf("failed to initialize session runtime")
		}
		inflight.rt.Touch()
		return inflight.rt, nil
	}

	inflight := &sessionCreateInFlight{done: make(chan struct{})}
	m.creating[id] = inflight
	m.mu.Unlock()

	rt, err := create(ctx)
	m.mu.Lock()
	delete(m.creating, id)

	var duplicate *serveRuntime
	var evicted *serveRuntime
	var retired *serveRuntime
	restoredReplaced := false
	switch {
	case err != nil:
		inflight.err = err
		if replaced != nil && !m.closed {
			if _, ok := m.sessions[id]; !ok {
				replaced.Touch()
				m.sessions[id] = replaced
				restoredReplaced = true
			}
		}
	case m.closed:
		inflight.err = errServeSessionManagerClosed
	default:
		if existing, ok := m.sessions[id]; ok {
			existing.Touch()
			inflight.rt = existing
			duplicate = rt
		} else {
			rt.Touch()
			evicted, inflight.err = m.makeRoomForNewSessionLocked()
			if inflight.err == nil {
				m.sessions[id] = rt
				inflight.rt = rt
			}
		}
	}
	if replaced != nil && !restoredReplaced {
		retired = replaced
	}
	close(inflight.done)
	m.mu.Unlock()

	if duplicate != nil {
		m.closeRuntime(duplicate)
	}
	m.retireRuntime(retired)
	m.retireRuntime(evicted)
	if inflight.err != nil {
		if rt != nil && inflight.rt == nil {
			m.closeRuntime(rt)
		}
		return nil, inflight.err
	}
	if inflight.rt == nil {
		return nil, fmt.Errorf("failed to initialize session runtime")
	}
	inflight.rt.Touch()
	return inflight.rt, nil
}

// BeginSwap creates and installs a candidate runtime for id while retaining the
// previous idle runtime for commit/rollback. State/approval/ask_user endpoints
// see the candidate after this returns. The previous runtime is not closed until
// commit; rollback restores it if the session still points at the candidate.
func (m *serveSessionManager) BeginSwap(ctx context.Context, id string, create func(context.Context) (*serveRuntime, error)) (candidate *serveRuntime, previous *serveRuntime, commit func(), rollback func(), err error) {
	for {
		m.mu.Lock()
		if _, reserved := m.reserved.Load(id); reserved {
			m.mu.Unlock()
			return nil, nil, nil, nil, errServeSessionBusy
		}
		if m.closed {
			m.mu.Unlock()
			return nil, nil, nil, nil, errServeSessionManagerClosed
		}
		previous = m.sessions[id]
		if previous != nil && previous.hasActiveActivity() {
			m.mu.Unlock()
			return nil, nil, nil, nil, errServeSessionBusy
		}
		if inflight, ok := m.creating[id]; ok {
			m.mu.Unlock()
			select {
			case <-inflight.done:
			case <-ctx.Done():
				return nil, nil, nil, nil, ctx.Err()
			}
			if inflight.err != nil {
				return nil, nil, nil, nil, inflight.err
			}
			continue
		}
		inflight := &sessionCreateInFlight{done: make(chan struct{})}
		m.creating[id] = inflight
		m.mu.Unlock()

		rt, createErr := create(ctx)

		m.mu.Lock()
		delete(m.creating, id)
		var evicted *serveRuntime
		var closeCandidate *serveRuntime
		if createErr != nil {
			inflight.err = createErr
		} else if m.closed {
			inflight.err = errServeSessionManagerClosed
			closeCandidate = rt
		} else {
			current := m.sessions[id]
			if current != previous {
				previous = current
			}
			if previous != nil && previous.hasActiveActivity() {
				inflight.err = errServeSessionBusy
				closeCandidate = rt
			} else {
				rt.Touch()
				if previous == nil {
					evicted, inflight.err = m.makeRoomForNewSessionLocked()
				}
				if inflight.err == nil {
					m.sessions[id] = rt
					inflight.rt = rt
				}
			}
		}
		close(inflight.done)
		m.mu.Unlock()

		if closeCandidate != nil {
			m.closeRuntime(closeCandidate)
		}
		m.retireRuntime(evicted)
		if inflight.err != nil {
			if rt != nil && closeCandidate == nil && inflight.rt == nil {
				m.closeRuntime(rt)
			}
			return nil, nil, nil, nil, inflight.err
		}
		if inflight.rt == nil {
			if rt != nil {
				m.closeRuntime(rt)
			}
			return nil, nil, nil, nil, fmt.Errorf("failed to initialize session runtime")
		}

		candidate = inflight.rt
		var once sync.Once
		commit = func() {
			once.Do(func() {
				m.mu.Lock()
				if current := m.sessions[id]; current == candidate {
					candidate.Touch()
				}
				m.mu.Unlock()
				if previous != nil && previous != candidate {
					m.retireRuntime(previous)
				}
			})
		}
		rollback = func() {
			once.Do(func() {
				restored := false
				m.mu.Lock()
				if current := m.sessions[id]; current == candidate {
					if previous != nil {
						previous.Touch()
						m.sessions[id] = previous
					} else {
						delete(m.sessions, id)
					}
					restored = true
				}
				m.mu.Unlock()
				_ = restored // kept for readability; candidate is retired regardless.
				m.closeRuntime(candidate)
			})
		}
		return candidate, previous, commit, rollback, nil
	}
}

// ActiveSessionIDs returns the set of session IDs that currently have an
// active run (activeInterrupt != nil). Unlike Get, this does NOT touch
// runtimes, so it won't extend their TTL.
func (m *serveSessionManager) ActiveSessionIDs() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]bool, len(m.sessions))
	for id, rt := range m.sessions {
		if rt.hasActiveRun() {
			result[id] = true
		}
	}
	return result
}

// UnresolvedInteractionSessionIDs returns runtimes waiting for an ask-user or
// approval decision without touching their TTL. These sessions belong to the
// active control plane even if a bounded recent-session query omits them.
func (m *serveSessionManager) UnresolvedInteractionSessionIDs() map[string]bool {
	m.mu.Lock()
	runtimes := make(map[string]*serveRuntime, len(m.sessions))
	for id, rt := range m.sessions {
		runtimes[id] = rt
	}
	m.mu.Unlock()
	result := make(map[string]bool)
	for id, rt := range runtimes {
		if rt == nil {
			continue
		}
		if len(rt.pendingAskUserPrompts()) > 0 || len(rt.pendingApprovalPrompts()) > 0 {
			result[id] = true
		}
	}
	return result
}

func (m *serveSessionManager) Close() {
	timeout := m.retirementTimeout
	if timeout <= 0 {
		timeout = defaultServeSessionRetirementTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	m.CloseContext(ctx)
}

func (m *serveSessionManager) CloseContext(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stopCh)
	sessions := make([]*serveRuntime, 0, len(m.sessions))
	for _, rt := range m.sessions {
		sessions = append(sessions, rt)
	}
	m.sessions = map[string]*serveRuntime{}
	m.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	for _, rt := range sessions {
		if m.onEvict != nil {
			m.onEvict(rt)
		}
		rt.CloseContext(ctx)
	}
}
