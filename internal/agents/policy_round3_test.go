package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMCPPolicyListedAgentsUseRegistryLookupNames(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	userRoot, err := GetUserAgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	search := t.TempDir()
	for _, entry := range []struct{ root, dir, declared string }{{userRoot, "trusted-lookup", "user-asserted"}, {search, "search-lookup", "search-asserted"}} {
		dir := filepath.Join(entry.root, entry.dir)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: "+entry.declared+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := NewRegistry(RegistryConfig{UseBuiltin: true, SearchPaths: []string{search}})
	if err != nil {
		t.Fatal(err)
	}
	for _, filtered := range []bool{false, true} {
		var listed []*Agent
		if filtered {
			listed, err = registry.ListBySource(SourceUser)
		} else {
			listed, err = registry.List()
		}
		if err != nil {
			t.Fatal(err)
		}
		var foundUser, foundSearch, foundBuiltin bool
		for _, agent := range listed {
			switch agent.Source {
			case SourceUser:
				foundUser = true
				if Reference(agent) != "trusted-lookup" || PolicyIdentity(agent) != "trusted-lookup" {
					t.Fatalf("listed user lost lookup: %+v", agent)
				}
			case SourceSearchPath:
				foundSearch = true
				if Reference(agent) != filepath.Join(search, "search-lookup") || PolicyIdentity(agent) != "" {
					t.Fatalf("listed search agent gained trust: %+v", agent)
				}
			case SourceBuiltin:
				foundBuiltin = true
				if Reference(agent) != agent.lookupName || agent.lookupName == "" || PolicyIdentity(agent) != agent.lookupName {
					t.Fatalf("listed builtin lost lookup: %+v", agent)
				}
			}
		}
		if !foundUser || !foundSearch || (!filtered && !foundBuiltin) {
			t.Fatal("missing listed source")
		}
	}
	for _, source := range []AgentSource{SourceUser, SourceBuiltin} {
		if Reference(&Agent{Name: "self-asserted", Source: source}) != "" {
			t.Fatal("unresolved agent persisted YAML name")
		}
	}
}
