package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestMCPPolicySearchPathReferenceCannotBecomeTrustedOnResume(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	// A relative search root must still persist an unambiguous path reference.
	searchDir := filepath.Join("search", "foo")
	if err := os.MkdirAll(searchDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(searchDir, "agent.yaml"), []byte("name: migration-data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Agents: config.AgentsConfig{SearchPaths: []string{"search"}}}
	agent, err := LoadAgent("foo", cfg)
	if err != nil {
		t.Fatal(err)
	}
	reference := agents.Reference(agent)
	if !filepath.IsAbs(reference) || agents.PolicyIdentity(agent) != "" {
		t.Fatalf("untrusted reference=%q identity=%q", reference, agents.PolicyIdentity(agent))
	}
	userRoot, err := agents.GetUserAgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(userRoot, "foo")
	if err := os.MkdirAll(userDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "agent.yaml"), []byte("name: foo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadAgent("foo", cfg)
	if err != nil || agents.PolicyIdentity(trusted) != "foo" {
		t.Fatalf("user agent not trusted: %v, %v", trusted, err)
	}
	resumed, err := LoadPersistedAgent(reference, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Name != "migration-data" || agents.Reference(resumed) != reference || agents.PolicyIdentity(resumed) != "" {
		t.Fatalf("search path acquired identity: %+v", resumed)
	}
	// Legacy ambiguous names cannot acquire trust while resolving to a search path.
	if err := os.RemoveAll(userDir); err != nil {
		t.Fatal(err)
	}
	legacy, err := LoadPersistedAgent("foo", cfg)
	if err != nil || agents.PolicyIdentity(legacy) != "" || agents.Reference(legacy) != reference {
		t.Fatalf("legacy reference failed: %+v %v", legacy, err)
	}
}
