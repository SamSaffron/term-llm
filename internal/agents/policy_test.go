package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyIdentity(t *testing.T) {
	for _, kind := range []string{"user", "builtin", "search-path", "local"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			root := filepath.Join(home, "term-llm", "agents")
			cfg := RegistryConfig{UseBuiltin: true}
			want := "reviewer"
			if kind == "search-path" || kind == "local" {
				root = t.TempDir()
				want = ""
				cfg.SearchPaths = []string{root}
			}
			var agent *Agent
			var err error
			if kind != "builtin" {
				dir := filepath.Join(root, "reviewer")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: self-declared\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "local" {
					agent, err = LoadFromPath(dir)
				}
			}
			registry, e := NewRegistry(cfg)
			if e != nil {
				t.Fatal(e)
			}
			if kind != "local" {
				agent, err = registry.Get("reviewer")
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := PolicyIdentity(agent); got != want {
				t.Fatalf("PolicyIdentity=%q, want %q", got, want)
			}
			if kind != "builtin" && agent.Name != "self-declared" {
				t.Fatal("test must retain self-declared name")
			}
			if kind == "search-path" && agent.Source.SourceName() != "search-path" {
				t.Fatal("dishonest source label")
			}
			if kind == "user" {
				if Reference(agent) != "reviewer" {
					t.Fatalf("reference=%q", Reference(agent))
				}
			}
			if kind == "local" || kind == "search-path" {
				if Reference(agent) != agent.SourcePath {
					t.Fatal("local reference lost path")
				}
			}
		})
	}
	if PolicyIdentity(nil) != "" || PolicyIdentity(&Agent{Name: "reviewer", Source: SourceUser}) != "" {
		t.Fatal("unresolved name granted identity")
	}
}
