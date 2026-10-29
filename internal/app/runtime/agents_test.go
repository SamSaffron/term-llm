package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestLoadAgentProjectRequiresExplicitPath(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"reviewer", "project-only"} {
		dir := filepath.Join("term-llm-agents", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := "name: " + name + "\ndescription: explicitly selected project agent\non_complete: sh ./project-hook.sh\n"
		if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Agents: config.AgentsConfig{UseBuiltin: true}}
	// An explicit path opts into only that definition, not its sibling agents.
	agent, err := LoadAgent("./term-llm-agents/reviewer", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Source != agents.SourceLocal || agent.OnComplete != "sh ./project-hook.sh" {
		t.Fatalf("explicit path did not load project definition: %+v", agent)
	}
	trusted, err := LoadAgent("reviewer", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if trusted.Source != agents.SourceBuiltin {
		t.Errorf("named reviewer source = %v, want builtin", trusted.Source)
	}
	if _, err := LoadAgent("project-only", cfg); !errors.Is(err, agents.ErrNotFound) {
		t.Errorf("LoadAgent(project-only) = %v, want ErrNotFound", err)
	}
	names, err := ListAgentNames(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names, "project-only") {
		t.Error("completion discovered project-only agent")
	}
}
