package hub

import (
	"errors"
	"fmt"
	"sort"
)

// Resolver enumerates nodes from one backing source (static config file,
// contain workspaces, the local UI-added store, ...). Implementations should
// be cheap to call: the hub re-resolves on each dashboard/API request so
// config edits and new workspaces are picked up without a restart.
type Resolver interface {
	// Source returns the stable label stamped onto nodes from this resolver.
	Source() string
	// Nodes returns the current nodes for this source.
	Nodes() ([]Node, error)
}

// Registry combines several resolvers into one lookup surface. Resolver order
// is precedence order: when two sources yield the same node ID, the earlier
// resolver wins and the later duplicate is dropped.
type Registry struct {
	resolvers []Resolver
}

// NewRegistry builds a registry over the given resolvers in precedence order.
func NewRegistry(resolvers ...Resolver) *Registry {
	return &Registry{resolvers: resolvers}
}

// Nodes returns all known nodes sorted by name (then ID), deduplicated by ID
// with earlier resolvers winning. A failing resolver does not hide the
// others' nodes: its error is joined into the returned error while the
// remaining sources still resolve, so one broken source never blanks the
// dashboard.
func (r *Registry) Nodes() ([]Node, error) {
	var (
		nodes []Node
		errs  []error
		seen  = map[string]bool{}
	)
	for _, res := range r.resolvers {
		batch, err := res.Nodes()
		if err != nil {
			errs = append(errs, fmt.Errorf("resolver %q: %w", res.Source(), err))
			continue
		}
		for _, n := range batch {
			if n.Source == "" {
				n.Source = res.Source()
			}
			if n.ID == "" || seen[n.ID] {
				continue
			}
			seen[n.ID] = true
			nodes = append(nodes, n)
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Name != nodes[j].Name {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].ID < nodes[j].ID
	})
	return nodes, errors.Join(errs...)
}

// Lookup resolves a single node by ID, honoring the same precedence as Nodes.
func (r *Registry) Lookup(id string) (Node, bool) {
	if id == "" {
		return Node{}, false
	}
	nodes, _ := r.Nodes()
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}
