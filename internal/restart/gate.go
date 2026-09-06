package restart

import "sync"

// Gate accounts for actual owned work, independently of cancellation or mode.
// Closing admission lets already admitted work (and its descendants) finish.
// It neither cancels work nor invents a resumable checkpoint.
// The zero value is usable.
type Gate struct {
	mu     sync.Mutex
	pauses int
	active int
}

// Enter admits a new root operation unless draining. The returned release is
// idempotent and must outlive all synchronous effects of the operation.
func (g *Gate) Enter() (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pauses != 0 {
		return nil, false
	}
	return g.trackLocked(), true
}

// TrackChild accounts for a descendant before its admitted parent releases.
// Descendants remain permitted during drain; rejecting them would lose work.
func (g *Gate) TrackChild() func() {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.trackLocked()
}

func (g *Gate) trackLocked() func() {
	g.active++
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); g.active--; g.mu.Unlock() }) }
}

// Pause closes new admission without affecting existing operations. Its rollback
// must be called if replacement fails. One lifecycle owner controls each gate.
func (g *Gate) Pause() func() {
	g.mu.Lock()
	g.pauses++
	g.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); g.pauses--; g.mu.Unlock() }) }
}

func (g *Gate) Drained() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pauses != 0 && g.active == 0
}
