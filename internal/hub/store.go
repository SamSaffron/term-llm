package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store persists UI-added nodes in a local JSON file (0600 — it holds node
// bearer tokens). It doubles as a Resolver so UI-added nodes resolve through
// the same Registry as config/contain nodes.
type Store struct {
	mu   sync.Mutex
	path string
}

// storeFile is the on-disk JSON shape.
type storeFile struct {
	Nodes []Node `json:"nodes"`
}

// NewStore returns a store backed by the given JSON file path. The file is
// created lazily on first Add.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

// Source implements Resolver.
func (s *Store) Source() string { return SourceLocal }

// Nodes implements Resolver. A missing file is an empty store, not an error.
func (s *Store) Nodes() ([]Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *Store) readLocked() ([]Node, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read hub node store: %w", err)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse hub node store %s: %w", s.path, err)
	}
	for i := range f.Nodes {
		f.Nodes[i].Source = SourceLocal
	}
	return f.Nodes, nil
}

func (s *Store) writeLocked(nodes []Node) error {
	data, err := json.MarshalIndent(storeFile{Nodes: nodes}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hub node store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create hub node store dir: %w", err)
	}
	// 0600: the store holds node bearer tokens. Chmod first so an existing
	// overly-permissive file is corrected as well as newly created files.
	if err := os.Chmod(s.path, 0o600); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secure hub node store permissions: %w", err)
	}
	if err := os.WriteFile(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write hub node store: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("secure hub node store permissions: %w", err)
	}
	return nil
}

// Add normalizes and persists a new node. The node ID must not collide with
// another stored node.
func (s *Store) Add(n Node) (Node, error) {
	n.Source = SourceLocal
	if err := n.Normalize(); err != nil {
		return Node{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes, err := s.readLocked()
	if err != nil {
		return Node{}, err
	}
	for _, existing := range nodes {
		if existing.ID == n.ID {
			return Node{}, fmt.Errorf("node id %q already exists", n.ID)
		}
	}
	nodes = append(nodes, n)
	if err := s.writeLocked(nodes); err != nil {
		return Node{}, err
	}
	return n, nil
}

// Remove deletes a stored node by ID. Removing an unknown ID is an error so
// the UI can surface a stale dashboard.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes, err := s.readLocked()
	if err != nil {
		return err
	}
	kept := nodes[:0]
	found := false
	for _, n := range nodes {
		if n.ID == id {
			found = true
			continue
		}
		kept = append(kept, n)
	}
	if !found {
		return fmt.Errorf("node %q not found in local store", id)
	}
	return s.writeLocked(kept)
}
