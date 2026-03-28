package cmd

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type serveSessionManager struct {
	ttl     time.Duration
	max     int
	factory func(context.Context) (*serveRuntime, error)
	onEvict func(rt *serveRuntime) // called when a session is evicted

	mu       sync.Mutex
	sessions map[string]*serveRuntime
	creating map[string]*sessionCreateInFlight
	closed   bool
	stopCh   chan struct{}
}

type sessionCreateInFlight struct {
	done chan struct{}
	rt   *serveRuntime
	err  error
}

func newServeSessionManager(ttl time.Duration, max int, factory func(context.Context) (*serveRuntime, error)) *serveSessionManager {
	m := &serveSessionManager{
		ttl:      ttl,
		max:      max,
		factory:  factory,
		sessions: make(map[string]*serveRuntime),
		creating: make(map[string]*sessionCreateInFlight),
		stopCh:   make(chan struct{}),
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
		if now.Sub(rt.LastUsed()) > m.ttl {
			delete(m.sessions, id)
			stale = append(stale, rt)
		}
	}
	m.mu.Unlock()

	for _, rt := range stale {
		if m.onEvict != nil {
			m.onEvict(rt)
		}
		rt.Close()
	}
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
		return nil, fmt.Errorf("session manager closed")
	}
	if rt, ok := m.sessions[id]; ok {
		rt.Touch()
		m.mu.Unlock()
		return rt, nil
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
		inflight.err = fmt.Errorf("session manager closed")
	default:
		if existing, ok := m.sessions[id]; ok {
			existing.Touch()
			inflight.rt = existing
			duplicate = rt
		} else {
			rt.Touch()
			if len(m.sessions) >= m.max {
				oldestID := ""
				var oldestTime time.Time
				for sid, srt := range m.sessions {
					t := srt.LastUsed()
					if oldestID == "" || t.Before(oldestTime) {
						oldestID = sid
						oldestTime = t
					}
				}
				if oldestID != "" {
					evicted = m.sessions[oldestID]
					delete(m.sessions, oldestID)
				}
			}
			m.sessions[id] = rt
			inflight.rt = rt
		}
	}
	close(inflight.done)
	m.mu.Unlock()

	if duplicate != nil {
		duplicate.Close()
	}
	if evicted != nil {
		if m.onEvict != nil {
			m.onEvict(evicted)
		}
		evicted.Close()
	}
	if inflight.err != nil {
		if rt != nil && inflight.rt == nil {
			rt.Close()
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
		return nil, fmt.Errorf("session manager closed")
	}
	if rt, ok := m.sessions[id]; ok {
		rt.Touch()
		m.mu.Unlock()
		return rt, nil
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
		inflight.err = fmt.Errorf("session manager closed")
	default:
		if existing, ok := m.sessions[id]; ok {
			existing.Touch()
			inflight.rt = existing
			duplicate = rt
		} else {
			rt.Touch()
			if len(m.sessions) >= m.max {
				oldestID := ""
				var oldestTime time.Time
				for sid, srt := range m.sessions {
					t := srt.LastUsed()
					if oldestID == "" || t.Before(oldestTime) {
						oldestID = sid
						oldestTime = t
					}
				}
				if oldestID != "" {
					evicted = m.sessions[oldestID]
					delete(m.sessions, oldestID)
				}
			}
			m.sessions[id] = rt
			inflight.rt = rt
		}
	}
	close(inflight.done)
	m.mu.Unlock()

	if duplicate != nil {
		duplicate.Close()
	}
	if evicted != nil {
		if m.onEvict != nil {
			m.onEvict(evicted)
		}
		evicted.Close()
	}
	if inflight.err != nil {
		if rt != nil && inflight.rt == nil {
			rt.Close()
		}
		return nil, inflight.err
	}
	if inflight.rt == nil {
		return nil, fmt.Errorf("failed to initialize session runtime")
	}
	inflight.rt.Touch()
	return inflight.rt, nil
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

func (m *serveSessionManager) Close() {
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

	for _, rt := range sessions {
		if m.onEvict != nil {
			m.onEvict(rt)
		}
		rt.Close()
	}
}
