package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

func orderNodes(ids ...string) []Node {
	nodes := make([]Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, Node{ID: id, Name: id, URL: "http://127.0.0.1:1", BasePath: "/chat", Token: "secret-" + id})
	}
	return nodes
}

func arrangedIDs(t *testing.T, s *NodeOrderStore, nodes []Node) []string {
	t.Helper()
	arranged, err := s.Arrange(nodes)
	if err != nil {
		t.Fatalf("Arrange: %v", err)
	}
	return nodeIDs(arranged)
}

func savedOrder(t *testing.T, path string) nodeOrderFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file nodeOrderFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("saved order is not JSON: %v\n%s", err, data)
	}
	return file
}

func TestNodeOrderFirstListingFreezesRegistryOrderPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub", "node-order.json")
	s := NewNodeOrderStore(path)
	if got := arrangedIDs(t, s, orderNodes("alpha", "beta", "gamma")); !slices.Equal(got, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("first listing = %v, want the registry order", got)
	}
	file := savedOrder(t, path)
	if file.Version != 1 || !slices.Equal(file.NodeIDs, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("saved order = %+v", file)
	}
	data, _ := os.ReadFile(path)
	for _, leak := range []string{"secret-", "http://", "/chat"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("order file contains %q: %s", leak, data)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("order file mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestNodeOrderNewNodesAppendAndRenamesNeverMove(t *testing.T) {
	s := NewNodeOrderStore(filepath.Join(t.TempDir(), "node-order.json"))
	arrangedIDs(t, s, orderNodes("beta", "gamma"))
	// The registry lists by name, so the new node arrives first; it still
	// appends after the nodes already placed.
	if got := arrangedIDs(t, s, orderNodes("alpha", "beta", "gamma")); !slices.Equal(got, []string{"beta", "gamma", "alpha"}) {
		t.Fatalf("after a new node = %v, want it appended", got)
	}
	renamed := orderNodes("beta", "gamma", "alpha")
	renamed[1].Name = "Aardvark"
	if got := arrangedIDs(t, s, renamed); !slices.Equal(got, []string{"beta", "gamma", "alpha"}) {
		t.Fatalf("after a rename = %v, want an unchanged order", got)
	}
	// Several nodes seen at once append in the registry's order.
	if got := arrangedIDs(t, s, orderNodes("alpha", "beta", "delta", "echo", "gamma")); !slices.Equal(got, []string{"beta", "gamma", "alpha", "delta", "echo"}) {
		t.Fatalf("after two new nodes = %v", got)
	}
}

func TestNodeOrderAbsentNodesKeepTheirPlace(t *testing.T) {
	s := NewNodeOrderStore(filepath.Join(t.TempDir(), "node-order.json"))
	arrangedIDs(t, s, orderNodes("alpha", "beta", "gamma", "delta"))

	// Beta disappears (a failing resolver, a deregistered reverse node) while
	// the others are rearranged; its slot stays reserved.
	present := orderNodes("alpha", "gamma", "delta")
	committed, err := s.Reorder(present, []string{"delta", "alpha", "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(committed, []string{"delta", "alpha", "gamma"}) {
		t.Fatalf("committed = %v, want only listed nodes in their new order", committed)
	}
	if got := savedOrder(t, s.Path()).NodeIDs; !slices.Equal(got, []string{"delta", "beta", "alpha", "gamma"}) {
		t.Fatalf("saved = %v, want beta to keep its slot", got)
	}
	if got := arrangedIDs(t, s, orderNodes("alpha", "beta", "delta", "gamma")); !slices.Equal(got, []string{"delta", "beta", "alpha", "gamma"}) {
		t.Fatalf("after beta returns = %v", got)
	}
}

func TestNodeOrderReorderPermutesOnlyListedSlots(t *testing.T) {
	cases := []struct {
		name   string
		listed []string
		want   []string
	}{
		{"move last to first", []string{"delta", "alpha", "beta", "gamma"}, []string{"delta", "alpha", "beta", "gamma"}},
		{"swap a subset", []string{"delta", "beta"}, []string{"alpha", "delta", "gamma", "beta"}},
		{"single id changes nothing", []string{"gamma"}, []string{"alpha", "beta", "gamma", "delta"}},
		{"ids are trimmed", []string{" beta ", "alpha"}, []string{"beta", "alpha", "gamma", "delta"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewNodeOrderStore(filepath.Join(t.TempDir(), "node-order.json"))
			nodes := orderNodes("alpha", "beta", "gamma", "delta")
			arrangedIDs(t, s, nodes)
			committed, err := s.Reorder(nodes, tc.listed)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(committed, tc.want) {
				t.Fatalf("committed = %v, want %v", committed, tc.want)
			}
			if got := arrangedIDs(t, s, nodes); !slices.Equal(got, tc.want) {
				t.Fatalf("listing after reorder = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNodeOrderRejectsInvalidRequestsWithoutWriting(t *testing.T) {
	tooMany := make([]string, MaxNodeOrderIDs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("n%d", i)
	}
	cases := []struct {
		name   string
		listed []string
		want   error
	}{
		{"empty", nil, ErrNodeOrderInvalid},
		{"too many", tooMany, ErrNodeOrderInvalid},
		{"duplicate", []string{"alpha", "beta", "alpha"}, ErrNodeOrderInvalid},
		{"blank", []string{"alpha", " "}, ErrNodeOrderInvalid},
		{"path segment", []string{"alpha", "../beta"}, ErrNodeOrderInvalid},
		{"unknown", []string{"alpha", "ghost"}, ErrNodeOrderUnknownNode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node-order.json")
			s := NewNodeOrderStore(path)
			if _, err := s.Reorder(orderNodes("beta", "alpha"), tc.listed); !errors.Is(err, tc.want) {
				t.Fatalf("Reorder error = %v, want %v", err, tc.want)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("rejected request wrote the order file (stat err %v)", err)
			}
		})
	}
}

func TestNodeOrderUnchangedReorderWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-order.json")
	compact := []byte(`{"version":1,"node_ids":["beta","alpha"]}`)
	if err := os.WriteFile(path, compact, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewNodeOrderStore(path)
	nodes := orderNodes("alpha", "beta")
	for _, listed := range [][]string{{"beta", "alpha"}, {"alpha"}} {
		committed, err := s.Reorder(nodes, listed)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(committed, []string{"beta", "alpha"}) {
			t.Fatalf("committed = %v", committed)
		}
	}
	if _, err := s.Arrange(nodes); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(compact) {
		t.Fatalf("unchanged order rewrote the file: %s", data)
	}
}

func TestNodeOrderUnreadableFileFallsBackAndIsReplacedByAReorder(t *testing.T) {
	cases := map[string][]byte{
		"malformed":           []byte(`{"version":1,"node_ids":["alpha"`),
		"wrong shape":         []byte(`["alpha","beta"]`),
		"empty":               {},
		"unsupported version": []byte(`{"version":2,"node_ids":["gamma","beta","alpha"]}`),
		"oversized":           []byte(`{"version":1,"node_ids":[]}` + strings.Repeat(" ", nodeOrderMaxBytes)),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node-order.json")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			s := NewNodeOrderStore(path)
			nodes := orderNodes("alpha", "beta", "gamma")
			arranged, err := s.Arrange(nodes)
			if !errors.Is(err, errNodeOrderCorrupt) {
				t.Fatalf("Arrange error = %v, want an unreadable order", err)
			}
			if got := nodeIDs(arranged); !slices.Equal(got, []string{"alpha", "beta", "gamma"}) {
				t.Fatalf("fallback order = %v, want the registry order", got)
			}
			if data, _ := os.ReadFile(path); string(data) != string(content) {
				t.Fatal("a listing rewrote an unreadable order file")
			}

			committed, err := s.Reorder(nodes, []string{"gamma", "alpha", "beta"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(committed, []string{"gamma", "alpha", "beta"}) {
				t.Fatalf("committed = %v", committed)
			}
			if backup, err := os.ReadFile(path + ".corrupt"); err != nil || string(backup) != string(content) {
				t.Fatalf("unreadable file was not kept aside: %v", err)
			}
			if got := arrangedIDs(t, s, nodes); !slices.Equal(got, []string{"gamma", "alpha", "beta"}) {
				t.Fatalf("listing after repair = %v", got)
			}
		})
	}
}

func TestNodeOrderIgnoresInvalidAndRepeatedSavedIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-order.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"node_ids":["gamma","../x","","gamma","alpha"],"future":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewNodeOrderStore(path)
	if got := arrangedIDs(t, s, orderNodes("alpha", "beta", "gamma")); !slices.Equal(got, []string{"gamma", "alpha", "beta"}) {
		t.Fatalf("listing = %v", got)
	}
	if got := savedOrder(t, path).NodeIDs; !slices.Equal(got, []string{"gamma", "alpha", "beta"}) {
		t.Fatalf("saved = %v, want the sanitized order plus the new node", got)
	}
}

func TestNodeOrderNeverSavesAnUnsavableID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-order.json")
	s := NewNodeOrderStore(path)
	nodes := []Node{{ID: "bad id"}, {ID: "alpha"}}
	if got := arrangedIDs(t, s, nodes); !slices.Equal(got, []string{"alpha", "bad id"}) {
		t.Fatalf("listing = %v, want the unsavable node last", got)
	}
	before, _ := os.ReadFile(path)
	if got := arrangedIDs(t, s, nodes); !slices.Equal(got, []string{"alpha", "bad id"}) {
		t.Fatalf("second listing = %v", got)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || strings.Contains(string(after), "bad id") {
		t.Fatalf("unsavable ID caused a rewrite: %s", after)
	}
}

func TestNodeOrderRetentionDropsLowestRankedAbsentIDsFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-order.json")
	ids := make([]string, 0, nodeOrderRetainedIDs)
	for i := 0; i < nodeOrderRetainedIDs; i++ {
		ids = append(ids, fmt.Sprintf("old-%04d", i))
	}
	data, _ := json.Marshal(nodeOrderFile{Version: 1, NodeIDs: ids})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewNodeOrderStore(path)
	// Two new nodes and one returning node exceed the cap by two: the two
	// lowest-ranked absent IDs go, while present nodes all stay.
	present := orderNodes("old-0999", "new-a", "new-b")
	arrangedIDs(t, s, present)
	saved := savedOrder(t, path).NodeIDs
	if len(saved) != nodeOrderRetainedIDs {
		t.Fatalf("saved %d IDs, want the cap %d", len(saved), nodeOrderRetainedIDs)
	}
	if slices.Contains(saved, "old-0997") || slices.Contains(saved, "old-0998") {
		t.Fatal("kept the lowest-ranked absent IDs")
	}
	for _, id := range []string{"old-0000", "old-0996", "old-0999", "new-a", "new-b"} {
		if !slices.Contains(saved, id) {
			t.Fatalf("saved order dropped %s", id)
		}
	}
	if got := arrangedIDs(t, s, present); !slices.Equal(got, []string{"old-0999", "new-a", "new-b"}) {
		t.Fatalf("listing = %v", got)
	}
}

func TestNodeOrderWritesArePrivateAndReplaceSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions and symlinks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "node-order.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"node_ids":["beta","alpha"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewNodeOrderStore(path)
	nodes := orderNodes("alpha", "beta")
	if _, err := s.Reorder(nodes, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after rewrite = %v, want 0600", info.Mode().Perm())
	}

	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"node_ids":["beta","alpha"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reorder(nodes, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != `{"version":1,"node_ids":["beta","alpha"]}` {
		t.Fatalf("write followed the symlink: %s", data)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("order path is still a symlink (err %v)", err)
	}
}

func TestNodeOrderConcurrentUseKeepsAValidPermutation(t *testing.T) {
	s := NewNodeOrderStore(filepath.Join(t.TempDir(), "node-order.json"))
	nodes := orderNodes("alpha", "beta", "gamma", "delta", "echo")
	orders := [][]string{
		{"echo", "delta", "gamma", "beta", "alpha"},
		{"beta", "alpha"},
		{"gamma", "echo", "alpha"},
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%4 == 0 {
				if _, err := s.Arrange(nodes); err != nil {
					t.Error(err)
				}
				return
			}
			if _, err := s.Reorder(nodes, orders[i%len(orders)]); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got := arrangedIDs(t, s, nodes)
	sorted := slices.Clone(got)
	slices.Sort(sorted)
	if !slices.Equal(sorted, []string{"alpha", "beta", "delta", "echo", "gamma"}) {
		t.Fatalf("final order %v is not a permutation of the nodes", got)
	}
}
