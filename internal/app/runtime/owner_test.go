package runtime

import (
	"context"
	"sync/atomic"
	"testing"
)

type ownedRuntimeStub struct {
	outstanding bool
	closed      chan struct{}
	drain       chan struct{}
	stopped     atomic.Bool
}

func (s *ownedRuntimeStub) HasOutstandingAgents() bool { return s.outstanding }
func (s *ownedRuntimeStub) DrainOwned(ctx context.Context) error {
	select {
	case <-s.drain:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *ownedRuntimeStub) ShutdownOwned(context.Context) error {
	s.stopped.Store(true)
	close(s.drain)
	return nil
}
func (s *ownedRuntimeStub) CloseOwned(context.Context, bool) { close(s.closed) }

func TestAgentHostOwnerWaitsForChildBeforeClosing(t *testing.T) {
	t.Parallel()
	owner := &AgentHostOwner{}
	rt := &ownedRuntimeStub{outstanding: true, closed: make(chan struct{}), drain: make(chan struct{})}
	var storeClosed atomic.Bool
	owner.Adopt(rt, func() { storeClosed.Store(true) })
	select {
	case <-rt.closed:
		t.Fatal("runtime closed while child was outstanding")
	default:
	}
	if err := owner.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rt.stopped.Load() || !storeClosed.Load() {
		t.Fatal("shutdown did not stop children and close their store")
	}
	select {
	case <-rt.closed:
	default:
		t.Fatal("runtime remained open after shutdown")
	}
}
