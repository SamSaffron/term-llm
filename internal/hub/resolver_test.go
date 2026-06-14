package hub

import (
	"fmt"
	"testing"
)

type fakeResolver struct {
	source string
	nodes  []Node
	err    error
}

func (f fakeResolver) Source() string         { return f.source }
func (f fakeResolver) Nodes() ([]Node, error) { return f.nodes, f.err }

func TestRegistryDedupesByPrecedence(t *testing.T) {
	r := NewRegistry(
		fakeResolver{source: "config", nodes: []Node{
			{ID: "shared", Name: "from-config", URL: "http://127.0.0.1:1"},
			{ID: "cfg-only", Name: "cfg", URL: "http://127.0.0.1:2"},
		}},
		fakeResolver{source: "contain", nodes: []Node{
			{ID: "shared", Name: "from-contain", URL: "http://127.0.0.1:3"},
			{ID: "ws", Name: "ws", URL: "http://127.0.0.1:4"},
		}},
	)
	nodes, err := r.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("len(nodes) = %d, want 3 (duplicate dropped)", len(nodes))
	}
	got, ok := r.Lookup("shared")
	if !ok || got.Name != "from-config" {
		t.Fatalf("Lookup(shared) = %+v, %v; want the config node (earlier resolver wins)", got, ok)
	}
}

func TestRegistrySoftFailsBrokenResolver(t *testing.T) {
	r := NewRegistry(
		fakeResolver{source: "config", err: fmt.Errorf("boom")},
		fakeResolver{source: "contain", nodes: []Node{{ID: "ok", Name: "ok", URL: "http://127.0.0.1:1"}}},
	)
	nodes, err := r.Nodes()
	if err == nil {
		t.Fatal("expected the broken resolver's error to surface")
	}
	if len(nodes) != 1 || nodes[0].ID != "ok" {
		t.Fatalf("nodes = %+v, want the surviving resolver's node", nodes)
	}
}

func TestRegistryStampsSource(t *testing.T) {
	r := NewRegistry(fakeResolver{source: "config", nodes: []Node{{ID: "a", Name: "a", URL: "http://127.0.0.1:1"}}})
	nodes, err := r.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0].Source != "config" {
		t.Fatalf("Source = %q, want stamped from resolver", nodes[0].Source)
	}
}

func TestRegistrySortsByName(t *testing.T) {
	r := NewRegistry(fakeResolver{source: "config", nodes: []Node{
		{ID: "b", Name: "zeta", URL: "http://127.0.0.1:1"},
		{ID: "a", Name: "alpha", URL: "http://127.0.0.1:2"},
	}})
	nodes, _ := r.Nodes()
	if nodes[0].Name != "alpha" || nodes[1].Name != "zeta" {
		t.Fatalf("nodes unsorted: %+v", nodes)
	}
}
