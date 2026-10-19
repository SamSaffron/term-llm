package filetrack

import (
	"context"
	"sync"
)

// backgroundGC runs at most once, and yields permanently when foreground work
// starts. Registering it synchronously prevents a late goroutine from starting
// maintenance after recording has already begun.
type backgroundGC struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

func (g *backgroundGC) start(run func(context.Context)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	g.done = make(chan struct{})
	go func() {
		defer close(g.done)
		defer cancel()
		run(ctx)
	}()
}

func (g *backgroundGC) stop(ctx context.Context) error {
	g.mu.Lock()
	g.stopped = true
	cancel, done := g.cancel, g.done
	g.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	// Prefer an already-drained worker even if the caller is canceled, too.
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StartBackgroundGC starts a best-effort startup sweep. Foreground recording
// cancels and drains it before writing, so maintenance cannot monopolize the
// SQLite writer while a run is starting. A sweep is not started if recording
// has already begun; callers can still use GC for explicit maintenance.
func (s *Store) StartBackgroundGC(sessionsDBPath string, maxAgeDays int) {
	s.backgroundGC.start(func(ctx context.Context) {
		_ = s.GC(ctx, sessionsDBPath, maxAgeDays)
	})
}
