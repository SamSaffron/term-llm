package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
)

// MaxNodeOrderIDs bounds one node reorder request.
const MaxNodeOrderIDs = 1000

const (
	nodeOrderVersion = 1
	// nodeOrderMaxBytes bounds how much of the order file is read; a larger
	// file is treated as corrupt rather than parsed.
	nodeOrderMaxBytes = 1 << 20
	// nodeOrderRetainedIDs bounds the saved order. IDs of nodes the registry
	// no longer lists are kept, so a node that returns (a fixed config file,
	// a recovered resolver, a reverse node re-registering after a restart)
	// regains its place, but only while the order holds at most this many IDs.
	nodeOrderRetainedIDs = 1000
)

var (
	// ErrNodeOrderInvalid means a reorder request was empty, oversized, or
	// listed an invalid node ID or the same node more than once.
	ErrNodeOrderInvalid = errors.New("hub: invalid node order")
	// ErrNodeOrderUnknownNode means a reorder request listed a node the
	// registry does not currently list.
	ErrNodeOrderUnknownNode = errors.New("hub: unknown node")

	// errNodeOrderCorrupt means the order file exists but is not a readable
	// order: oversized, not JSON, or an unsupported version.
	errNodeOrderCorrupt = errors.New("hub: unreadable node order")
)

// nodeOrderFile is the on-disk shape: node IDs in dashboard order.
type nodeOrderFile struct {
	Version int      `json:"version"`
	NodeIDs []string `json:"node_ids"`
}

// NodeOrderStore persists the dashboard's node order in a small private JSON
// file, separate from every node source, so arranging nodes never rewrites a
// config file or the node store. The file holds node IDs only (never tokens,
// URLs, or names), mode 0600.
//
// Every node gets a place the first time the Hub lists it: the first listing
// freezes the registry's order, and nodes seen later are appended. The list
// therefore never moves for renames, activity, or probes. A node the registry
// stops listing keeps its place until it returns.
type NodeOrderStore struct {
	mu   sync.Mutex
	path string
}

// NewNodeOrderStore returns a store backed by the given JSON file path. The
// file is created on the first listing that sees a node.
func NewNodeOrderStore(path string) *NodeOrderStore {
	return &NodeOrderStore{path: path}
}

// Path returns the backing file path.
func (s *NodeOrderStore) Path() string { return s.path }

// Arrange returns nodes in the saved order and saves a place at the end for
// each node it has not seen before, in the order given (the registry's name
// order). If the saved order cannot be read, nodes are returned unchanged
// with the error and the file is left alone. If saving newly seen nodes
// fails, the arranged nodes are still returned with the error.
func (s *NodeOrderStore) Arrange(nodes []Node) ([]Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.readLocked()
	if err != nil {
		return nodes, err
	}
	order, added := appendUnseen(saved, nodeIDs(nodes))
	if added {
		err = s.writeLocked(retainNodeOrder(order, nodeIDSet(nodes)))
	}
	return arrangeNodes(nodes, order), err
}

// Reorder moves the listed node IDs, in the given order, into the positions
// they already occupy in the saved order; every other node, including ones
// the registry does not currently list, keeps its position. Every listed ID
// must belong to one of nodes. It returns the committed order of nodes. The
// request is atomic, and an unchanged order writes nothing. An unreadable
// order file is kept beside the new one with a ".corrupt" suffix and replaced.
func (s *NodeOrderStore) Reorder(nodes []Node, orderedIDs []string) ([]string, error) {
	ids, err := normalizeNodeOrderIDs(orderedIDs)
	if err != nil {
		return nil, err
	}
	present := nodeIDSet(nodes)
	for _, id := range ids {
		if !present[id] {
			return nil, fmt.Errorf("%w: %s", ErrNodeOrderUnknownNode, id)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.readLocked()
	corrupt := errors.Is(err, errNodeOrderCorrupt)
	if err != nil && !corrupt {
		return nil, err
	}
	merged, _ := appendUnseen(saved, nodeIDs(nodes))
	next := permuteNodeOrder(merged, ids)
	if !corrupt && slices.Equal(next, saved) {
		return presentNodeOrder(next, present), nil
	}
	if corrupt {
		// Best effort: keep the unreadable bytes for inspection. If the
		// rename fails, the write below replaces them anyway.
		_ = os.Rename(s.path, s.path+".corrupt")
	}
	if err := s.writeLocked(retainNodeOrder(next, present)); err != nil {
		return nil, err
	}
	return presentNodeOrder(next, present), nil
}

// readLocked returns the saved order. A missing file is an empty order.
// Invalid and repeated IDs are dropped; an oversized, malformed, or
// unsupported file is errNodeOrderCorrupt.
func (s *NodeOrderStore) readLocked() ([]string, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read hub node order: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, nodeOrderMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read hub node order: %w", err)
	}
	if len(data) > nodeOrderMaxBytes {
		return nil, fmt.Errorf("%w %s: larger than %d bytes", errNodeOrderCorrupt, s.path, nodeOrderMaxBytes)
	}
	var file nodeOrderFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("%w %s: %v", errNodeOrderCorrupt, s.path, err)
	}
	if file.Version != nodeOrderVersion {
		return nil, fmt.Errorf("%w %s: unsupported version %d", errNodeOrderCorrupt, s.path, file.Version)
	}
	ids := make([]string, 0, len(file.NodeIDs))
	seen := make(map[string]bool, len(file.NodeIDs))
	for _, id := range file.NodeIDs {
		if ValidateID(id) != nil || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// writeLocked atomically replaces the order file with ids. The file is
// application-owned state: the write never follows a symlink at the path and
// always leaves the file private.
func (s *NodeOrderStore) writeLocked(ids []string) error {
	data, err := json.MarshalIndent(nodeOrderFile{Version: nodeOrderVersion, NodeIDs: ids}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hub node order: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create hub node order dir: %w", err)
	}
	if err := config.WriteFileAtomicallyNoFollow(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write hub node order: %w", err)
	}
	return nil
}

// normalizeNodeOrderIDs trims a reorder request's IDs and rejects a request
// that is empty, names more than MaxNodeOrderIDs nodes, or names an invalid
// node ID or the same node twice.
func normalizeNodeOrderIDs(orderedIDs []string) ([]string, error) {
	if len(orderedIDs) == 0 {
		return nil, fmt.Errorf("%w: no nodes listed", ErrNodeOrderInvalid)
	}
	if len(orderedIDs) > MaxNodeOrderIDs {
		return nil, fmt.Errorf("%w: at most %d nodes may be ordered at once", ErrNodeOrderInvalid, MaxNodeOrderIDs)
	}
	ids := make([]string, 0, len(orderedIDs))
	seen := make(map[string]bool, len(orderedIDs))
	for _, raw := range orderedIDs {
		id := strings.TrimSpace(raw)
		if err := ValidateID(id); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNodeOrderInvalid, err)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: node %s is listed more than once", ErrNodeOrderInvalid, id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// appendUnseen returns saved followed by the valid IDs in ids it does not
// contain, in their given order, and whether any were appended. An ID that
// could not be saved is never appended, so it cannot cause a write on every
// listing.
func appendUnseen(saved, ids []string) ([]string, bool) {
	known := make(map[string]bool, len(saved)+len(ids))
	order := make([]string, 0, len(saved)+len(ids))
	for _, id := range saved {
		known[id] = true
		order = append(order, id)
	}
	added := false
	for _, id := range ids {
		if known[id] || ValidateID(id) != nil {
			continue
		}
		known[id] = true
		order = append(order, id)
		added = true
	}
	return order, added
}

// permuteNodeOrder places ids, in order, into the positions they occupy
// within order. Every other entry keeps its position.
func permuteNodeOrder(order, ids []string) []string {
	position := make(map[string]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	slots := make([]int, 0, len(ids))
	for _, id := range ids {
		slots = append(slots, position[id])
	}
	sort.Ints(slots)
	next := slices.Clone(order)
	for i, id := range ids {
		next[slots[i]] = id
	}
	return next
}

// retainNodeOrder bounds order for saving: every present ID stays, and absent
// ones stay in rank order while the order holds at most nodeOrderRetainedIDs
// entries, so the lowest-ranked absent IDs are dropped first.
func retainNodeOrder(order []string, present map[string]bool) []string {
	if len(order) <= nodeOrderRetainedIDs {
		return order
	}
	absent := nodeOrderRetainedIDs
	for _, id := range order {
		if present[id] {
			absent--
		}
	}
	kept := make([]string, 0, nodeOrderRetainedIDs)
	for _, id := range order {
		if present[id] {
			kept = append(kept, id)
		} else if absent > 0 {
			kept = append(kept, id)
			absent--
		}
	}
	return kept
}

// presentNodeOrder filters order to the IDs in present.
func presentNodeOrder(order []string, present map[string]bool) []string {
	out := make([]string, 0, len(present))
	for _, id := range order {
		if present[id] {
			out = append(out, id)
		}
	}
	return out
}

// arrangeNodes returns nodes sorted by their IDs' positions in order. A node
// missing from order follows every ordered one, keeping its given order.
func arrangeNodes(nodes []Node, order []string) []Node {
	position := make(map[string]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	rank := func(id string) int {
		if i, ok := position[id]; ok {
			return i
		}
		return len(order)
	}
	arranged := slices.Clone(nodes)
	sort.SliceStable(arranged, func(i, j int) bool {
		return rank(arranged[i].ID) < rank(arranged[j].ID)
	})
	return arranged
}

func nodeIDs(nodes []Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return ids
}

func nodeIDSet(nodes []Node) map[string]bool {
	set := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		set[n.ID] = true
	}
	return set
}
