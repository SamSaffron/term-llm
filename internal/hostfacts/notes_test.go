package hostfacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostKey(t *testing.T) {
	if got := HostKey("My.Host.Example"); got != "my" {
		t.Fatalf("HostKey=%q", got)
	}
}
func TestHostNotes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	hostname, _ := os.Hostname()
	path := filepath.Join(home, "term-llm", "hosts", HostKey(hostname)+".md")
	missing := Notes()
	if !strings.Contains(missing, path) {
		t.Fatalf("missing hint=%q", missing)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("keep {{host_facts}} literal"), 0600); err != nil {
		t.Fatal(err)
	}
	got := Notes()
	if !strings.Contains(got, "# Host notes") || !strings.Contains(got, "{{host_facts}}") {
		t.Fatalf("notes=%q", got)
	}
}
