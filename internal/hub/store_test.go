package hub

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStoreAddListRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub", "nodes.json")
	s := NewStore(path)

	nodes, err := s.Nodes()
	if err != nil || len(nodes) != 0 {
		t.Fatalf("empty store Nodes() = %v, %v", nodes, err)
	}

	added, err := s.Add(Node{Name: "Jarvis", URL: "http://127.0.0.1:8081/chat", Token: "tkn"})
	if err != nil {
		t.Fatal(err)
	}
	if added.ID != "jarvis" || added.Source != SourceLocal {
		t.Fatalf("added = %+v", added)
	}

	// Token must persist (it is injected server-side on proxying) and the
	// file must be private: it holds bearer tokens.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("store file mode = %v, want 0600", fi.Mode().Perm())
		}
		// A rewrite tightens an existing permissive store file too.
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Add(Node{ID: "other", URL: "http://127.0.0.1:9000/chat"}); err != nil {
			t.Fatal(err)
		}
		fi, err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("store file mode after rewrite = %v, want 0600", fi.Mode().Perm())
		}
		if err := s.Remove("other"); err != nil {
			t.Fatal(err)
		}
	}

	// A second store over the same path sees the persisted node.
	nodes, err = NewStore(path).Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != "jarvis" || nodes[0].Token != "tkn" {
		t.Fatalf("persisted nodes = %+v", nodes)
	}

	if _, err := s.Add(Node{ID: "jarvis", URL: "http://127.0.0.1:9000/chat"}); err == nil {
		t.Fatal("duplicate id added, want error")
	}

	if err := s.Remove("jarvis"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("jarvis"); err == nil {
		t.Fatal("removing missing node succeeded, want error")
	}
	nodes, err = s.Nodes()
	if err != nil || len(nodes) != 0 {
		t.Fatalf("after remove Nodes() = %v, %v", nodes, err)
	}
}

func TestStoreRejectsInvalidNode(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "nodes.json"))
	if _, err := s.Add(Node{Name: "x", URL: "not-a-url"}); err == nil {
		t.Fatal("invalid url added, want error")
	}
}
