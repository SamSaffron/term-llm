package hub

import (
	"errors"
	"sync/atomic"
)

// Generations are process-local: no reverse sockets survive a Hub restart.
// Neither the store lifetime nor registry generation is serialized on Node.
var nextIdentityGeneration atomic.Uint64

type nodeSecurityIdentity struct {
	id, source, connection, url, basePath, token string
	lifetime                                     uint64
}

func (n Node) securityIdentity() nodeSecurityIdentity {
	return nodeSecurityIdentity{n.ID, n.Source, n.Connection, n.URL, n.BasePath, n.Token, n.lifetime}
}

// SameIdentity reports whether both snapshots belong to the same effective
// registry generation. Plain nodes not obtained from Registry never match.
// Display names and delegation policy are deliberately not transport identity.
func (n Node) SameIdentity(other Node) bool {
	return n.generation != 0 && n.generation == other.generation && n.securityIdentity() == other.securityIdentity()
}

func (s *Store) stampNodesLocked(nodes []Node) {
	next := make(map[string]Node, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		previous, exists := s.identities[n.ID]
		// Incoming snapshots may carry a lifetime; it is never authoritative.
		n.lifetime = previous.lifetime
		if !exists || n.securityIdentity() != previous.securityIdentity() {
			n.lifetime = nextIdentityGeneration.Add(1)
		}
		n.generation = 0
		next[n.ID] = *n
	}
	s.identities = next
}

func (r *Registry) stampNodesLocked(nodes []Node) {
	next := make(map[string]Node, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		previous, exists := r.identities[n.ID]
		n.generation = previous.generation
		if !exists || n.securityIdentity() != previous.securityIdentity() {
			n.generation = nextIdentityGeneration.Add(1)
		}
		next[n.ID] = *n
	}
	r.identities = next
	for _, observe := range r.observers {
		observe(nodes)
	}
}

// Observe registers an effective-node snapshot observer. Observers run during
// resolution and after Mutate, under the registry lock; they MUST NOT call back
// into Registry or mutate the snapshot. Store mutations must use Mutate to make
// notification and validation atomic. Lock order is Registry before Store.
func (r *Registry) Observe(observe func([]Node)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observers = append(r.observers, observe)
}

// Mutate serializes a backing store operation with identity validation, then
// synchronously notifies observers of the effective registry state. The callback
// MUST NOT reenter Registry. Resolution errors use the same fail-closed snapshot
// semantics as Lookup and do not mask the mutation's own result.
func (r *Registry) Mutate(mutate func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := mutate()
	_, _ = r.nodesLocked()
	return err
}

// WithCurrent re-resolves and validates an authenticated snapshot, then invokes
// use under the registry lock. The callback MUST NOT reenter Registry or perform
// network I/O. Callers must bind subsequent I/O to a revocable connection rather
// than looking it up again by ID after validation.
func (r *Registry) WithCurrent(node Node, use func(Node) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	nodes, _ := r.nodesLocked()
	for _, current := range nodes {
		if current.ID == node.ID && node.SameIdentity(current) {
			return use(current)
		}
	}
	return errors.New("node registration is no longer current")
}
