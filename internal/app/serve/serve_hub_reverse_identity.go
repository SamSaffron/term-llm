package serve

import (
	"errors"

	"github.com/samsaffron/term-llm/internal/hub"
)

var errHubReverseStale = errors.New("reverse connection registration is no longer current")

// reconcile runs under the registry lock. Never acquire the registry lock while
// holding m.mu: the order is Registry -> manager. Closing outside m.mu lets the
// pointer-checked detach leave a newly attached/current socket untouched.
func (m *hubReverseManager) reconcile(nodes []hub.Node) {
	current := make(map[string]hub.Node, len(nodes))
	for _, node := range nodes {
		current[node.ID] = node
	}
	var stale []*hubReverseConnection
	m.mu.RLock()
	for id, c := range m.conns {
		if !c.node.SameIdentity(current[id]) {
			stale = append(stale, c)
		}
	}
	m.mu.RUnlock()
	for _, c := range stale {
		c.closeWithError(errHubReverseStale)
	}
}

func (m *hubReverseManager) connection(node hub.Node) (*hubReverseConnection, error) {
	var c *hubReverseConnection
	err := m.registry.WithCurrent(node, func(hub.Node) error {
		m.mu.RLock()
		defer m.mu.RUnlock()
		c = m.conns[node.ID]
		if c == nil || !node.SameIdentity(c.node) {
			return errHubReverseStale
		}
		return nil
	})
	return c, err
}

// validate runs after acquiring the connection's write gate, immediately before
// writing a request. It never reselects a replacement socket. Once validated,
// the operation is authorized before a concurrent mutation; mutation closes the
// exact old socket before returning and aborts its pending work. Network writes
// are outside the registry lock so a slow node cannot block unrelated nodes or
// prevent its own revocation. Bytes already sent cannot be recalled.
func (c *hubReverseConnection) validate() error {
	if c.manager == nil {
		return errHubReverseStale
	}
	current, err := c.manager.connection(c.node)
	if err != nil {
		return err
	}
	if current != c {
		return errHubReverseStale
	}
	return nil
}

func (s *hubServer) addHubNode(node hub.Node) (stored hub.Node, err error) {
	err = s.registry.Mutate(func() error {
		stored, err = s.store.Add(node)
		return err
	})
	return stored, err
}

func (s *hubServer) upsertHubNode(node hub.Node) (stored hub.Node, created bool, err error) {
	err = s.registry.Mutate(func() error {
		stored, created, err = s.store.Upsert(node)
		return err
	})
	return stored, created, err
}

func (s *hubServer) removeHubNode(id string) error {
	return s.registry.Mutate(func() error { return s.store.Remove(id) })
}

func (s *hubServer) unregisterHubNode(id string) (removed bool, err error) {
	err = s.registry.Mutate(func() error {
		removed, err = s.store.RemoveIfExists(id)
		return err
	})
	return removed, err
}
