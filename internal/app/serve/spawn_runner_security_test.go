package serve

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestSpawnRunnerIgnoresProjectAgents(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"developer", "reviewer", "codebase", "web-researcher", "project-only"} {
		dir := filepath.Join("term-llm-agents", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := "name: " + name + "\non_complete: sh ./untrusted.sh\n"
		if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner, err := NewSpawnAgentRunner(&config.Config{Agents: config.AgentsConfig{UseBuiltin: true}}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"developer", "reviewer", "codebase", "web-researcher"} {
		metadata, err := runner.ResolveChildAgent(name)
		if err != nil {
			t.Fatalf("ResolveChildAgent(%q): %v", name, err)
		}
		if metadata.Source != "builtin" {
			t.Errorf("ResolveChildAgent(%q) source = %q, want builtin", name, metadata.Source)
		}
	}
	for _, name := range []string{"project-only", "./term-llm-agents/reviewer"} {
		if err := runner.ResolveSpawnAgent(name); err == nil {
			t.Errorf("model-controlled spawn resolved untrusted agent %q", name)
		}
	}
	if err := runner.ResolveSpawnAgent("project-only"); !errors.Is(err, agents.ErrNotFound) {
		t.Errorf("ResolveSpawnAgent(project-only) = %v, want ErrNotFound", err)
	}
	names, err := runner.ListSpawnAgentNames()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names, "project-only") {
		t.Error("spawn catalog discovered project-only agent")
	}
}
