package server

import (
	"strings"
	"sync"
)

// StartsEventStream reports whether a frame opens a long-lived event stream.
func (r ReverseResponse) StartsEventStream() bool {
	return r.Type == ReverseFrameResponseStart && r.Error == "" &&
		strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "text/event-stream")
}

// ReverseSlot holds one admission token of a reverse operation, from either
// the request or the event-stream pool. Exactly one owner returns it: the
// operation when it finishes, or its queued cancel once the frame is written.
type ReverseSlot struct {
	mu      sync.Mutex
	release func()
}

// Hold records how to return the token.
func (s *ReverseSlot) Hold(release func()) {
	s.mu.Lock()
	s.release = release
	s.mu.Unlock()
}

// Take transfers the token to the caller; nil means it was already returned.
func (s *ReverseSlot) Take() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	release := s.release
	s.release = nil
	return release
}

// Release returns the token if it is still held.
func (s *ReverseSlot) Release() {
	if release := s.Take(); release != nil {
		release()
	}
}

// MoveTo rehomes a still-held token into pool and frees its current slot. It
// reports false, changing nothing, when pool is full or the token is gone.
func (s *ReverseSlot) MoveTo(pool chan struct{}) bool {
	select {
	case pool <- struct{}{}:
	default:
		return false
	}
	s.mu.Lock()
	old := s.release
	if old != nil {
		s.release = func() { <-pool }
	}
	s.mu.Unlock()
	if old == nil {
		<-pool
		return false
	}
	old()
	return true
}

// ReverseCancel is a queued cancel frame plus the slot it keeps held: until
// the frame is written the node may still be running the operation.
type ReverseCancel struct {
	ID      string
	Release func() // nil when the operation held no slot
}

// Done returns the held slot.
func (c ReverseCancel) Done() {
	if c.Release != nil {
		c.Release()
	}
}

// ReverseCancels is a connection's bounded cancel backlog for its single
// cancel writer. Each entry holds its operation's slot until written, so freed
// slots cannot let queued callers add work faster than cancels drain, and the
// backlog cannot outgrow the slot pools that size it.
type ReverseCancels struct {
	mu     sync.Mutex
	closed bool
	queue  chan ReverseCancel
}

// NewReverseCancels sizes the backlog; size should cover every slot pool.
func NewReverseCancels(size int) *ReverseCancels {
	return &ReverseCancels{queue: make(chan ReverseCancel, size)}
}

// Queue delivers cancels to the writer.
func (q *ReverseCancels) Queue() <-chan ReverseCancel { return q.queue }

// Push queues cancel; after Close it returns the slot at once. It reports false
// only when the backlog is full, leaving the slot with the caller.
func (q *ReverseCancels) Push(cancel ReverseCancel) bool {
	q.mu.Lock()
	closed, queued := q.closed, false
	if !closed {
		select {
		case q.queue <- cancel:
			queued = true
		default:
		}
	}
	q.mu.Unlock()
	if closed {
		cancel.Done()
	}
	return closed || queued
}

// Close stops queuing and returns the slots of cancels never written. Only the
// writer calls it, once it stops reading Queue.
func (q *ReverseCancels) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	for {
		select {
		case cancel := <-q.queue:
			cancel.Done()
		default:
			return
		}
	}
}
