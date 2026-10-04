package cmd

import (
	"context"
	"sync"
	"time"
)

// agentHostOwner holds per-turn runtimes while their detached children finish.
// In particular the child provider, approval manager and session store must not
// be closed when the parent's model turn returns.
type agentHostOwner struct {
	mu       sync.Mutex
	runs     map[*serveRuntime]chan struct{}
	stopping bool
}

func (o *agentHostOwner) adopt(rt *serveRuntime, closeStore func()) {
	if rt == nil {
		if closeStore != nil {
			closeStore()
		}
		return
	}
	if rt.spawnRunner == nil || len(rt.spawnRunner.OutstandingAgentIDs()) == 0 {
		rt.closeContext(context.Background(), false)
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
		rt.closeContext(ctx, false)
		cancel()
		if closeStore != nil {
			closeStore()
		}
		return
	}
	if o.runs == nil {
		o.runs = make(map[*serveRuntime]chan struct{})
	}
	o.runs[rt] = done
	o.mu.Unlock()
	go func() {
		defer close(done)
		_ = rt.spawnRunner.Drain(context.Background())
		rt.closeContext(context.Background(), false)
		if closeStore != nil {
			closeStore()
		}
		o.mu.Lock()
		delete(o.runs, rt)
		o.mu.Unlock()
	}()
}

func (o *agentHostOwner) Shutdown(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	o.stopping = true
	runs := make(map[*serveRuntime]chan struct{}, len(o.runs))
	for rt, done := range o.runs {
		runs[rt] = done
	}
	o.mu.Unlock()
	var wg sync.WaitGroup
	for rt := range runs {
		wg.Add(1)
		go func(rt *serveRuntime) { defer wg.Done(); _ = rt.spawnRunner.Shutdown(ctx) }(rt)
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
