package cmd

import (
	"context"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/restart"
)

const (
	hubHealthInterval    = 10 * time.Second
	hubHealthFreshness   = 30 * time.Second
	hubHealthProbeBudget = 3 * time.Second
	hubHealthConcurrency = 8
)

// Credentials and routing are part of the identity: replacing a node must not
// reuse an observation made against its old backend or authentication policy.
// This key is private and must never be included in public diagnostics.
type hubHealthIdentity struct {
	id, url, basePath, connection, token string
}

func hubNodeHealthIdentity(n hub.Node) hubHealthIdentity {
	return hubHealthIdentity{n.ID, n.URL, n.BasePath, n.Connection, n.Token}
}

type hubHealthObservation struct {
	startedAt time.Time
	status    hub.Status
}

type hubHealthCache struct {
	mu      sync.Mutex
	entries map[hubHealthIdentity]hubHealthObservation
}

// retain both prunes removed/replaced nodes and admits the current identities.
// A late probe cannot reinsert an identity removed during its request.
func (c *hubHealthCache) retain(nodes []hub.Node) {
	current := make(map[hubHealthIdentity]hubHealthObservation, len(nodes))
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range nodes {
		key := hubNodeHealthIdentity(n)
		current[key] = c.entries[key]
	}
	c.entries = current
}

func (c *hubHealthCache) publish(ctx context.Context, n hub.Node, startedAt time.Time, status hub.Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := hubNodeHealthIdentity(n)
	previous, ok := c.entries[key]
	if ctx.Err() != nil || !ok || !startedAt.After(previous.startedAt) {
		return
	}
	c.entries[key] = hubHealthObservation{startedAt: startedAt, status: status}
}

func (c *hubHealthCache) status(n hub.Node, now time.Time) hub.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[hubNodeHealthIdentity(n)]
	if !ok || entry.startedAt.IsZero() || now.Sub(entry.startedAt) >= hubHealthFreshness {
		return hub.Status{State: "unknown"}
	}
	return entry.status
}

func (s *hubServer) sidebarNodeHealth(n hub.Node, now time.Time) hub.Status {
	if !n.UsesReverseConnection() {
		return s.healthCache.status(n, now)
	}
	// Websocket presence is already the source of reverse reachability, even
	// when a reverse health request fails. Never reuse a previous socket's
	// cached connected status (or metadata) after disconnect/reconnect.
	connected, connectedAt, lastSeen := s.reverse.status(n.ID)
	if !connected {
		return hub.Status{State: "disconnected", Error: "waiting for reverse connection", Details: map[string]string{"connection": "reverse"}}
	}
	return hub.Status{Reachable: true, State: "connected", Details: map[string]string{
		"connection": "reverse", "connected_at": connectedAt.Format(time.RFC3339), "last_seen": lastSeen.Format(time.RFC3339),
	}}
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
