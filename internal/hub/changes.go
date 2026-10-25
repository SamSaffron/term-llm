package hub

import (
	"context"
	"sync"
	"time"
)

// ChangeFeed is a process-local version counter for Hub dashboard state
// (attention projection, delegation ledger). Clients long-poll on it and
// refetch their panels when the version moves. Versions reset on restart;
// waiters compare for inequality, so a reset reads as a change.
type ChangeFeed struct {
	mu      sync.Mutex
	version uint64
	ch      chan struct{}
	closed  bool
}

// NewChangeFeed returns an empty feed.
func NewChangeFeed() *ChangeFeed { return &ChangeFeed{ch: make(chan struct{})} }

// Bump advances the version and wakes every waiter. Nil-safe.
func (f *ChangeFeed) Bump() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.version++
	close(f.ch)
	f.ch = make(chan struct{})
}

// Close releases every current and future waiter (server shutdown).
func (f *ChangeFeed) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.ch)
	}
}

// Version returns the current version.
func (f *ChangeFeed) Version() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version
}

// Wait blocks until the version differs from since, the timeout elapses, or
// ctx ends, then returns the current version.
func (f *ChangeFeed) Wait(ctx context.Context, since uint64, timeout time.Duration) uint64 {
	f.mu.Lock()
	version, ch, closed := f.version, f.ch, f.closed
	f.mu.Unlock()
	if version != since || closed {
		return version
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-ctx.Done():
	}
	return f.Version()
}
