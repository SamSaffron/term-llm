package hub

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRegistryStoreIdentityLifetimes(t *testing.T) {
	for _, mutation := range []string{"rename", "rotate", "rotate_restore", "delete_upsert", "delete_add"} {
		t.Run(mutation, func(t *testing.T) {
			store := NewStore(filepath.Join(t.TempDir(), "nodes.json"))
			node := Node{ID: "node", Name: "original", Connection: "reverse", BasePath: "/chat", Token: "same-token"}
			if _, err := store.Add(node); err != nil {
				t.Fatal(err)
			}
			registry := NewRegistry(store)
			before, _ := registry.Lookup(node.ID)
			if !before.SameIdentity(before) || node.SameIdentity(before) {
				t.Fatal("only issued snapshots should have transport identity")
			}
			if err := registry.Mutate(func() error {
				switch mutation {
				case "rename":
					node.Name = "renamed"
				case "rotate":
					node.Token = "new-token"
				case "rotate_restore":
					rotated := node
					rotated.Token = "temporary-token"
					if _, _, err := store.Upsert(rotated); err != nil {
						return err
					}
				default:
					// Deliberately do NOT resolve the registry between removal
					// and creation: the Store must remember the lifetime change.
					if err := store.Remove(node.ID); err != nil {
						return err
					}
				}
				if mutation == "delete_add" {
					_, err := store.Add(node)
					return err
				}
				_, _, err := store.Upsert(node)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			after, _ := registry.Lookup(node.ID)
			wantSame := mutation == "rename"
			if before.SameIdentity(after) != wantSame {
				t.Fatalf("same generation = %v, want %v", before.SameIdentity(after), wantSame)
			}
			used := false
			err := registry.WithCurrent(before, func(Node) error { used = true; return nil })
			if (err == nil) != wantSame || used != wantSame {
				t.Fatalf("stale snapshot validation err=%v callback=%v", err, used)
			}
			if err := registry.WithCurrent(after, func(Node) error { return nil }); err != nil {
				t.Fatalf("current snapshot rejected: %v", err)
			}
		})
	}
}

func TestRegistryIdentityChangesAndObserver(t *testing.T) {
	resolver := &identityTestResolver{nodes: []Node{{ID: "node", Connection: "reverse", BasePath: "/chat", Token: "one"}}}
	registry := NewRegistry(resolver)
	var observed Node
	registry.Observe(func(nodes []Node) {
		if len(nodes) == 1 {
			observed = nodes[0]
		} else {
			observed = Node{}
		}
	})
	original, _ := registry.Lookup("node")
	if !original.SameIdentity(observed) {
		t.Fatal("observer did not see issued snapshot")
	}
	for _, field := range []string{"token", "source", "connection", "basepath", "url", "absent"} {
		before, _ := registry.Lookup("node")
		node := resolver.nodes[0]
		switch field {
		case "token":
			node.Token += "-rotated"
		case "source":
			node.Source = SourceContain
		case "connection":
			node.Connection = "direct"
		case "basepath":
			node.BasePath = "/new"
		case "url":
			node.URL = "http://new.invalid"
		}
		resolver.nodes = []Node{node}
		if field == "absent" {
			resolver.nodes = nil
		}
		used := false
		if err := registry.WithCurrent(before, func(Node) error { used = true; return nil }); err == nil || used {
			t.Fatalf("%s change did not revoke snapshot", field)
		}
		if observed.SameIdentity(before) {
			t.Fatalf("%s observer retained revoked snapshot", field)
		}
	}
}

func TestRegistryMutationFailurePreservesIdentity(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "nodes.json"))
	node := Node{ID: "node", Connection: "reverse", BasePath: "/chat", Token: "secret"}
	if _, err := store.Add(node); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(store)
	before, _ := registry.Lookup(node.ID)
	failure := errors.New("mutation rejected")
	if err := registry.Mutate(func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	after, _ := registry.Lookup(node.ID)
	if !before.SameIdentity(after) {
		t.Fatal("failed mutation revoked current registration")
	}
	jsonData, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	yamlData, err := yaml.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{jsonData, yamlData} {
		for _, private := range []string{"generation", "lifetime"} {
			if strings.Contains(string(data), private) {
				t.Fatalf("private identity serialized: %s", data)
			}
		}
	}
}

func TestRegistryShadowedStoreMutationPreservesEffectiveIdentity(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "nodes.json"))
	node := Node{ID: "shared", Connection: "reverse", BasePath: "/chat", Token: "config-token"}
	registry := NewRegistry(&identityTestResolver{nodes: []Node{node}}, store)
	before, _ := registry.Lookup(node.ID)
	node.Token = "local-token"
	if err := registry.Mutate(func() error {
		_, err := store.Add(node)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func() error{
		func() error {
			node.Token = "rotated-local-token"
			_, _, err := store.Upsert(node)
			return err
		},
		func() error { return store.Remove(node.ID) },
	} {
		if err := registry.Mutate(mutate); err != nil {
			t.Fatal(err)
		}
		after, _ := registry.Lookup(node.ID)
		if !before.SameIdentity(after) {
			t.Fatal("shadowed local store mutation revoked the current config node")
		}
	}
}

type identityTestResolver struct{ nodes []Node }

func (*identityTestResolver) Source() string           { return SourceConfig }
func (r *identityTestResolver) Nodes() ([]Node, error) { return append([]Node(nil), r.nodes...), nil }
