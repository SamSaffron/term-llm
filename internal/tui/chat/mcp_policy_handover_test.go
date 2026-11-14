package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/term-llm/term-llm/internal/agents"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/tools"
	"gopkg.in/yaml.v3"
)

func TestMCPPolicyScriptHandoverPersistsSearchPathReference(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	dir := filepath.Join(root, "foo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: migration-data\nhandover_script: ./handover.sh\nmcp: [{name: restricted}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Agents: config.AgentsConfig{SearchPaths: []string{root}}}
	registry, err := agents.NewRegistry(agents.RegistryConfig{SearchPaths: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := registry.Get("foo")
	if err != nil {
		t.Fatal(err)
	}
	previous := runHandoverScriptForCmd
	runHandoverScriptForCmd = func(context.Context, *tools.ApprovalManager, *agents.Agent, string, []tools.TranscriptEntry) (string, error) {
		return "handover document", nil
	}
	t.Cleanup(func() { runHandoverScriptForCmd = previous })
	msg := handoverScriptCmd(context.Background(), nil, target, "source", target, "", true, "")().(handoverDoneMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if msg.agentName != dir {
		t.Fatalf("script persisted YAML name instead of source reference: %q", msg.agentName)
	}
	model := newCmdTestModel(&mockStore{})
	model.config = cfg
	sess := model.buildHandoverSession(&msg, target)
	if sess.Agent != dir {
		t.Fatalf("session agent=%q want %q", sess.Agent, dir)
	}
	resumed, err := agents.LoadFromPath(sess.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if agents.PolicyIdentity(resumed) != "" {
		t.Fatal("script handover acquired trusted identity")
	}
	policyRoot := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.MkdirAll(policyRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(policyRoot, "config.yaml"), []byte("mcp: {servers: {restricted: {agents: [migration-data]}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := mcp.NewManagerWithConfig(&mcp.Config{Servers: map[string]mcp.ServerConfig{"restricted": {}}}, mcp.WithPolicyIdentity(agents.PolicyIdentity(resumed)))
	if manager.CheckAccess("restricted") == nil {
		t.Fatal("search-path YAML name granted access after handover")
	}
	// Even a stale pending display name cannot override resolved provenance.
	msg.agentName = "migration-data"
	if got := model.buildHandoverSession(&msg, target).Agent; got != dir {
		t.Fatalf("stale pending name overrode reference: %q", got)
	}
}

// registeredHandoverTestAgent gives handover fixtures the same registry-owned
// reference as production. A synthetic SourceBuiltin label is not provenance.
func registeredHandoverTestAgent(t *testing.T, fixture *agents.Agent) *agents.Agent {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, err := agents.GetUserAgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, fixture.Name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "system.md"), []byte(fixture.SystemPrompt), 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := agents.NewRegistry(agents.RegistryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := registry.Get(fixture.Name)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}
