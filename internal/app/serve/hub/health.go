package hub

import (
	"context"
	"sync"
	"time"

	corehub "github.com/samsaffron/term-llm/internal/hub"
)

const (
	HealthInterval    = 10 * time.Second
	HealthFreshness   = 30 * time.Second
	HealthProbeBudget = 3 * time.Second
	HealthConcurrency = 8
)

// healthIdentity: credentials and routing are part of the identity, so
// replacing a node never reuses an observation made against its old backend
// or authentication policy. The key is private and must never be exposed.
type healthIdentity struct {
	id, url, basePath, connection, token string
}

func nodeHealthIdentity(n corehub.Node) healthIdentity {
	return healthIdentity{n.ID, n.URL, n.BasePath, n.Connection, n.Token}
}

type healthObservation struct {
	startedAt time.Time
	status    corehub.Status
}

// HealthCache holds recent node health observations. The zero value is ready
// to use after Retain.
type HealthCache struct {
	mu      sync.Mutex
	entries map[healthIdentity]healthObservation
}

// Retain both prunes removed/replaced nodes and admits the current identities.
// A late probe cannot reinsert an identity removed during its request.
func (c *HealthCache) Retain(nodes []corehub.Node) {
	current := make(map[healthIdentity]healthObservation, len(nodes))
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range nodes {
		key := nodeHealthIdentity(n)
		current[key] = c.entries[key]
	}
	c.entries = current
}

// Publish records a probe result unless the context ended, the node was
// removed, or a newer observation exists.
func (c *HealthCache) Publish(ctx context.Context, n corehub.Node, startedAt time.Time, status corehub.Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := nodeHealthIdentity(n)
	previous, ok := c.entries[key]
	if ctx.Err() != nil || !ok || !startedAt.After(previous.startedAt) {
		return
	}
	c.entries[key] = healthObservation{startedAt: startedAt, status: status}
}

// Status returns the fresh observation for n, or an "unknown" status.
func (c *HealthCache) Status(n corehub.Node, now time.Time) corehub.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[nodeHealthIdentity(n)]
	if !ok || entry.startedAt.IsZero() || now.Sub(entry.startedAt) >= HealthFreshness {
		return corehub.Status{State: "unknown"}
	}
	return entry.status
}

// ReverseStatus is the status of a node reached over a reverse websocket.
// Websocket presence is the source of reverse reachability, so cached probe
// results are never reused across disconnect/reconnect.
func ReverseStatus(connected bool, connectedAt, lastSeen time.Time) corehub.Status {
	if !connected {
		return corehub.Status{State: "disconnected", Error: "waiting for reverse connection", Details: map[string]string{"connection": "reverse"}}
	}
	return corehub.Status{Reachable: true, State: "connected", Details: map[string]string{
		"connection": "reverse", "connected_at": connectedAt.Format(time.RFC3339), "last_seen": lastSeen.Format(time.RFC3339),
	}}
}

// Len reports how many node identities are currently retained.
func (c *HealthCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
