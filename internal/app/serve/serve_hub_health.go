package serve

import (
	"context"
	"sync"
	"time"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/restart"
)

const (
	hubHealthInterval    = apphub.HealthInterval
	hubHealthFreshness   = apphub.HealthFreshness
	hubHealthProbeBudget = apphub.HealthProbeBudget
	hubHealthConcurrency = apphub.HealthConcurrency
)

// hubHealthCache adapts the extracted hub health cache for cmd call sites.
type hubHealthCache struct{ c apphub.HealthCache }

func (c *hubHealthCache) retain(nodes []hub.Node) { c.c.Retain(nodes) }

func (c *hubHealthCache) publish(ctx context.Context, n hub.Node, startedAt time.Time, status hub.Status) {
	c.c.Publish(ctx, n, startedAt, status)
}

func (c *hubHealthCache) len() int { return c.c.Len() }

func (c *hubHealthCache) status(n hub.Node, now time.Time) hub.Status { return c.c.Status(n, now) }

func (s *hubServer) sidebarNodeHealth(n hub.Node, now time.Time) hub.Status {
	if !n.UsesReverseConnection() {
		return s.healthCache.status(n, now)
	}
	connected, connectedAt, lastSeen := s.reverse.status(n.ID)
	return apphub.ReverseStatus(connected, connectedAt, lastSeen)
}

func (s *hubServer) startHealthMonitor() {
	if s.registry == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.healthCancel = cancel
	s.healthWG.Add(1)
	go func() {
		defer s.healthWG.Done()
		ticker := time.NewTicker(hubHealthInterval)
		defer ticker.Stop()
		for {
			s.collectNodeHealth(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// collectNodeHealth owns one restart activity through all probes and cache
// publication. Workers bound concurrency; each direct request has its own
// deadline. Cycles never overlap, even for unusually large registries.
func (s *hubServer) collectNodeHealth(ctx context.Context) {
	ctx, release, err := restart.Default.Activity(ctx)
	if err != nil {
		return
	}
	defer release()
	if ctx.Err() != nil {
		return
	}
	nodes, _ := s.registry.Nodes()
	s.healthCache.retain(nodes)
	work := make(chan hub.Node)
	var wg sync.WaitGroup
	for range min(hubHealthConcurrency, len(nodes)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				if ctx.Err() != nil {
					return
				}
				if n.UsesReverseConnection() {
					continue
				}
				startedAt := time.Now()
				probeCtx, cancel := context.WithTimeout(ctx, hubHealthProbeBudget)
				status := s.prober.Probe(probeCtx, n)
				cancel()
				// A probe deadline is an offline observation; cancellation of
				// the monitor/restart activity is not a health observation.
				s.healthCache.publish(ctx, n, startedAt, status)
			}
		}()
	}
	for _, n := range nodes {
		select {
		case work <- n:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return
		}
	}
	close(work)
	wg.Wait()
}
