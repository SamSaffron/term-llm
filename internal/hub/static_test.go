package hub

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigYAML(t *testing.T) {
	nodes, err := ParseConfig([]byte(`
nodes:
  - name: Jarvis
    url: http://127.0.0.1:8081/chat
    token: secret-1
  - id: edge
    url: https://edge.example.com
    base_path: /ui
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("len(nodes) = %d, want 2", len(nodes))
	}
	if nodes[0].ID != "jarvis" || nodes[0].Name != "Jarvis" || nodes[0].Token != "secret-1" {
		t.Errorf("node 0 = %+v", nodes[0])
	}
	if nodes[0].URL != "http://127.0.0.1:8081" || nodes[0].BasePath != "/chat" {
		t.Errorf("node 0 url/base = %q %q", nodes[0].URL, nodes[0].BasePath)
	}
	if nodes[0].Source != SourceConfig {
		t.Errorf("node 0 source = %q, want config", nodes[0].Source)
	}
	if nodes[1].ID != "edge" || nodes[1].BasePath != "/ui" || nodes[1].Token != "" {
		t.Errorf("node 1 = %+v", nodes[1])
	}
}

func TestParseConfigJSON(t *testing.T) {
	nodes, err := ParseConfig([]byte(`{"nodes":[{"name":"alpha","url":"http://127.0.0.1:8082/chat","token":"tkn"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != "alpha" || nodes[0].Token != "tkn" {
		t.Fatalf("nodes = %+v", nodes)
	}
}

func TestParseConfigRejectsDuplicateIDs(t *testing.T) {
	_, err := ParseConfig([]byte(`
nodes:
  - id: same
    url: http://127.0.0.1:8081/chat
  - id: same
    url: http://127.0.0.1:8082/chat
`))
	if err == nil {
		t.Fatal("duplicate ids parsed, want error")
	}
}

func TestParseConfigRejectsBadURL(t *testing.T) {
	if _, err := ParseConfig([]byte("nodes:\n  - id: x\n    url: not-a-url\n")); err == nil {
		t.Fatal("bad url parsed, want error")
	}
}

func TestStaticResolverReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.yaml")
	if err := os.WriteFile(path, []byte("nodes:\n  - name: a\n    url: http://127.0.0.1:8081/chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewStaticResolver(path)
	if r.Source() != SourceConfig {
		t.Errorf("Source = %q", r.Source())
	}
	nodes, err := r.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != "a" {
		t.Fatalf("nodes = %+v", nodes)
	}

	// Edits are picked up without restart: the file is re-read per call.
	if err := os.WriteFile(path, []byte("nodes:\n  - name: a\n    url: http://127.0.0.1:8081/chat\n  - name: b\n    url: http://127.0.0.1:8082/chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nodes, err = r.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("after edit len(nodes) = %d, want 2", len(nodes))
	}
}

func TestStaticResolverMissingFile(t *testing.T) {
	r := NewStaticResolver(filepath.Join(t.TempDir(), "missing.yaml"))
	if _, err := r.Nodes(); err == nil {
		t.Fatal("missing config file resolved, want error")
	}
}
