package runtime

import (
	"context"
	"sync"
	"time"
)

// AgentHostRuntime is the narrow lifecycle seam implemented by the serve runtime.
// Ownership only needs outstanding-child state, drain/shutdown and final close;
// it must not depend on the HTTP server or session preparation internals.
type AgentHostRuntime interface {
	HasOutstandingAgents() bool
	DrainOwned(context.Context) error
	ShutdownOwned(context.Context) error
	CloseOwned(context.Context, bool)
}

// AgentHostOwner holds per-turn runtimes while detached children finish.
// In particular the child provider, approval manager and session store must not
// be closed when the parent's model turn returns.
type AgentHostOwner struct {
	mu       sync.Mutex
	runs     map[AgentHostRuntime]chan struct{}
	stopping bool
}

func (o *AgentHostOwner) Adopt(rt AgentHostRuntime, closeStore func()) {
	if rt == nil {
		if closeStore != nil {
			closeStore()
		}
		return
	}
	if !rt.HasOutstandingAgents() {
		rt.CloseOwned(context.Background(), false)
		if closeStore != nil {
			closeStore()
		}
		return
	}
	done := make(chan struct{})
	o.mu.Lock()
	if o.stopping {
		o.mu.Unlock()
		// Shutdown has already snapshotted its runs. Do not let this child
		// escape that barrier or close its store before cancellation.
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		rt.CloseOwned(ctx, false)
		cancel()
		if closeStore != nil {
			closeStore()
		}
		return
	}
	if o.runs == nil {
		o.runs = make(map[AgentHostRuntime]chan struct{})
	}
	o.runs[rt] = done
	o.mu.Unlock()
	go func() {
		defer close(done)
		_ = rt.DrainOwned(context.Background())
		rt.CloseOwned(context.Background(), false)
		if closeStore != nil {
			closeStore()
		}
		o.mu.Lock()
		delete(o.runs, rt)
		o.mu.Unlock()
	}()
}

func (o *AgentHostOwner) Shutdown(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	o.stopping = true
	runs := make(map[AgentHostRuntime]chan struct{}, len(o.runs))
	for rt, done := range o.runs {
		runs[rt] = done
	}
	o.mu.Unlock()
	var wg sync.WaitGroup
	for rt := range runs {
		wg.Add(1)
		go func(rt AgentHostRuntime) { defer wg.Done(); _ = rt.ShutdownOwned(ctx) }(rt)
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		for _, done := range runs {
			<-done
		}
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
