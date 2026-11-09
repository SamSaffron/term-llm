package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestMCPPolicyLegacyBareReferencesResumeWithoutAmbiguousTrust(t *testing.T) {
	for _, kind := range []string{"search", "user-and-search", "user", "builtin", "missing", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			userRoot, err := agents.GetUserAgentsDir()
			if err != nil {
				t.Fatal(err)
			}
			searchRoot := t.TempDir()
			name := "foo"
			cfg := &config.Config{Agents: config.AgentsConfig{UseBuiltin: true, SearchPaths: []string{searchRoot}}}
			write := func(root, body string) {
				dir := filepath.Join(root, name)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "search" || kind == "user-and-search" {
				write(searchRoot, "name: foo\n")
			}
			if kind == "user" || kind == "user-and-search" {
				write(userRoot, "name: foo\n")
			}
			if kind == "invalid" {
				write(searchRoot, "name: [invalid\n")
			}
			if kind == "builtin" {
				name = "reviewer"
			}
			agent, err := LoadPersistedAgent(name, cfg)
			if kind == "invalid" {
				if err == nil {
					t.Fatal("invalid definition resumed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				if agent != nil {
					t.Fatal("missing definition resolved")
				}
				return
			}
			want := ""
			if kind == "user" || kind == "builtin" {
				want = name
			}
			if got := agents.PolicyIdentity(agent); got != want {
				t.Fatalf("legacy identity=%q want=%q", got, want)
			}
			if kind == "search" && agents.Reference(agent) != filepath.Join(searchRoot, name) {
				t.Fatalf("legacy search reference=%q", agents.Reference(agent))
			}
			if kind == "user-and-search" {
				fresh, err := LoadAgent(name, cfg)
				if err != nil || agents.PolicyIdentity(fresh) != name {
					t.Fatalf("withholding legacy trust mutated fresh registry loads: %v", err)
				}
			}
		})
	}
}
