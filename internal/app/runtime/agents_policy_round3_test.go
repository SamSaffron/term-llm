package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/term-llm/term-llm/internal/agents"
	"github.com/term-llm/term-llm/internal/config"
)

func TestMCPPolicyLegacyYAMLNamesCannotAcquireTrustedIdentity(t *testing.T) {
	for _, kind := range []string{"user", "builtin", "malformed", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			name := "migration-data"
			if kind == "builtin" {
				name = "developer"
			}
			if kind != "builtin" {
				root, err := agents.GetUserAgentsDir()
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(root, name)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: "+name+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			search := t.TempDir()
			dir := filepath.Join(search, "helper")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "agent.yaml")
			if kind == "unreadable" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				body := "name: " + name + "\n"
				if kind == "malformed" {
					body = "name: [broken"
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{Agents: config.AgentsConfig{UseBuiltin: true, SearchPaths: []string{search}}}
			fresh, err := LoadAgent(name, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if agents.PolicyIdentity(fresh) != name {
				t.Fatal("fixture must resolve trusted registry name")
			}
			resumed, err := LoadPersistedAgent(name, cfg)
			if kind == "malformed" || kind == "unreadable" {
				if err == nil {
					t.Fatal("could not prove provenance but resumed with trust")
				}
				return
			}
			if err != nil || resumed == nil || agents.PolicyIdentity(resumed) != "" {
				t.Fatalf("legacy alias acquired trusted identity: %+v %v", resumed, err)
			}
			if agents.PolicyIdentity(fresh) != name {
				t.Fatal("legacy suppression mutated fresh agent")
			}
		})
	}
}
