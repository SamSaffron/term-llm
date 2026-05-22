package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestRegistry_Get(t *testing.T) {
	// Create temp directories
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "user-agents")
	localDir := filepath.Join(tmpDir, "local-agents")

	if err := os.MkdirAll(filepath.Join(userDir, "user-agent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(localDir, "local-agent"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create agent files
	userYAML := `name: user-agent
description: "User agent"`
	if err := os.WriteFile(filepath.Join(userDir, "user-agent", "agent.yaml"), []byte(userYAML), 0644); err != nil {
		t.Fatal(err)
	}

	localYAML := `name: local-agent
description: "Local agent"`
	if err := os.WriteFile(filepath.Join(localDir, "local-agent", "agent.yaml"), []byte(localYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create registry with custom paths
	r := &Registry{
		useBuiltin: true,
		cache:      make(map[string]*Agent),
		searchPaths: []searchPath{
			{path: localDir, source: SourceLocal},
			{path: userDir, source: SourceUser},
		},
	}

	// Get local agent
	agent, err := r.Get("local-agent")
	if err != nil {
		t.Fatalf("Get(local-agent): %v", err)
	}
	if agent.Name != "local-agent" {
		t.Errorf("Name = %q, want %q", agent.Name, "local-agent")
	}
	if agent.Source != SourceLocal {
		t.Errorf("Source = %v, want %v", agent.Source, SourceLocal)
	}

	// Get user agent
	agent, err = r.Get("user-agent")
	if err != nil {
		t.Fatalf("Get(user-agent): %v", err)
	}
	if agent.Name != "user-agent" {
		t.Errorf("Name = %q, want %q", agent.Name, "user-agent")
	}
	if agent.Source != SourceUser {
		t.Errorf("Source = %v, want %v", agent.Source, SourceUser)
	}

	// Get builtin agent
	agent, err = r.Get("reviewer")
	if err != nil {
		t.Fatalf("Get(reviewer): %v", err)
	}
	if agent.Name != "reviewer" {
		t.Errorf("Name = %q, want %q", agent.Name, "reviewer")
	}
	if agent.Source != SourceBuiltin {
		t.Errorf("Source = %v, want %v", agent.Source, SourceBuiltin)
	}

	// Get non-existent agent
	_, err = r.Get("nonexistent")
	if err == nil {
		t.Error("Get(nonexistent) should return error")
	}
}

func TestRegistry_List(t *testing.T) {
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "user-agents")

	// Create two user agents
	for _, name := range []string{"alpha", "beta"} {
		agentDir := filepath.Join(userDir, name)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatal(err)
		}
		yaml := "name: " + name
		if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
	}

	r := &Registry{
		useBuiltin: true,
		cache:      make(map[string]*Agent),
		searchPaths: []searchPath{
			{path: userDir, source: SourceUser},
		},
	}

	agents, err := r.List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}

	// Should have user agents + builtin agents
	if len(agents) < 2+len(builtinAgentNames) {
		t.Errorf("len(agents) = %d, expected at least %d", len(agents), 2+len(builtinAgentNames))
	}

	// Check that agents are sorted by source (builtin > user > local) then by name within source
	for i := 1; i < len(agents); i++ {
		prev, curr := agents[i-1], agents[i]
		if prev.Source != curr.Source {
			// Source should be in descending order (Builtin=2 > User=1 > Local=0)
			if prev.Source < curr.Source {
				t.Errorf("agents not sorted by source: %s (%v) before %s (%v)",
					prev.Name, prev.Source, curr.Name, curr.Source)
			}
		} else {
			// Within same source, names should be in ascending order
			if prev.Name > curr.Name {
				t.Errorf("agents not sorted by name within source %v: %s > %s",
					prev.Source, prev.Name, curr.Name)
			}
		}
	}

	// Verify builtin agents come first
	if len(agents) > 0 && agents[0].Source != SourceBuiltin {
		t.Errorf("first agent should be builtin, got source %v", agents[0].Source)
	}

	// Verify user agents "alpha" and "beta" exist and come after builtins
	foundUser := 0
	for _, a := range agents {
		if a.Source == SourceUser && (a.Name == "alpha" || a.Name == "beta") {
			foundUser++
		}
	}
	if foundUser != 2 {
		t.Errorf("expected 2 user agents (alpha, beta), found %d", foundUser)
	}
}

func TestRegistry_ListNames(t *testing.T) {
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "user-agents")

	// Create two user agents
	for _, name := range []string{"alpha", "beta"} {
		agentDir := filepath.Join(userDir, name)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatal(err)
		}
		yaml := "name: " + name
		if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
	}

	r := &Registry{
		useBuiltin: true,
		cache:      make(map[string]*Agent),
		searchPaths: []searchPath{
			{path: userDir, source: SourceUser},
		},
	}

	names, err := r.ListNames()
	if err != nil {
		t.Fatalf("ListNames(): %v", err)
	}

	// Should have user agents + builtin agents
	if len(names) < 2+len(builtinAgentNames) {
		t.Errorf("len(names) = %d, expected at least %d", len(names), 2+len(builtinAgentNames))
	}

	// Check that names are sorted
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("names not sorted: %s > %s", names[i-1], names[i])
		}
	}

	// Check that alpha and beta are present
	hasAlpha, hasBeta := false, false
	for _, name := range names {
		if name == "alpha" {
			hasAlpha = true
		}
		if name == "beta" {
			hasBeta = true
		}
	}
	if !hasAlpha || !hasBeta {
		t.Errorf("missing user agents: alpha=%v, beta=%v", hasAlpha, hasBeta)
	}
}

func TestRegistry_Shadowing(t *testing.T) {
	tmpDir := t.TempDir()
	localDir := filepath.Join(tmpDir, "local-agents")

	// Create a local agent that shadows the builtin "reviewer"
	agentDir := filepath.Join(localDir, "reviewer")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	yaml := `name: reviewer
description: "Custom reviewer"`
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	r := &Registry{
		useBuiltin: true,
		cache:      make(map[string]*Agent),
		searchPaths: []searchPath{
			{path: localDir, source: SourceLocal},
		},
	}

	// Should get the local version, not builtin
	agent, err := r.Get("reviewer")
	if err != nil {
		t.Fatalf("Get(reviewer): %v", err)
	}
	if agent.Source != SourceLocal {
		t.Errorf("Source = %v, want %v (should shadow builtin)", agent.Source, SourceLocal)
	}
	if agent.Description != "Custom reviewer" {
		t.Errorf("Description = %q, want %q", agent.Description, "Custom reviewer")
	}
}

func TestRegistry_UseBuiltinFalse(t *testing.T) {
	r := &Registry{
		useBuiltin:  false,
		cache:       make(map[string]*Agent),
		searchPaths: []searchPath{},
	}

	// Should not find builtin agents
	_, err := r.Get("reviewer")
	if err == nil {
		t.Error("Get(reviewer) should fail when useBuiltin=false")
	}

	agents, _ := r.List()
	for _, a := range agents {
		if a.Source == SourceBuiltin {
			t.Errorf("List() returned builtin agent %q when useBuiltin=false", a.Name)
		}
	}
}

func TestCreateAgentDir(t *testing.T) {
	tmpDir := t.TempDir()

	err := CreateAgentDir(tmpDir, "my-agent")
	if err != nil {
		t.Fatalf("CreateAgentDir: %v", err)
	}

	// Check files were created
	agentPath := filepath.Join(tmpDir, "my-agent", "agent.yaml")
	if _, err := os.Stat(agentPath); os.IsNotExist(err) {
		t.Error("agent.yaml was not created")
	}

	systemPath := filepath.Join(tmpDir, "my-agent", "system.md")
	if _, err := os.Stat(systemPath); os.IsNotExist(err) {
		t.Error("system.md was not created")
	}

	// Try to load the created agent
	agent, err := LoadFromDir(filepath.Join(tmpDir, "my-agent"), SourceUser)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if agent.Name != "my-agent" {
		t.Errorf("Name = %q, want %q", agent.Name, "my-agent")
	}
}

func TestIsAgentDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create valid agent dir
	validDir := filepath.Join(tmpDir, "valid")
	if err := os.MkdirAll(validDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(validDir, "agent.yaml"), []byte("name: valid"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create invalid dir (no agent.yaml)
	invalidDir := filepath.Join(tmpDir, "invalid")
	if err := os.MkdirAll(invalidDir, 0755); err != nil {
		t.Fatal(err)
	}

	if !isAgentDir(validDir) {
		t.Error("isAgentDir(validDir) = false, want true")
	}
	if isAgentDir(invalidDir) {
		t.Error("isAgentDir(invalidDir) = true, want false")
	}
	if isAgentDir(filepath.Join(tmpDir, "nonexistent")) {
		t.Error("isAgentDir(nonexistent) = true, want false")
	}
}

func TestRegistry_GetConcurrentWithPreferenceUpdates(t *testing.T) {
	r := &Registry{
		useBuiltin: true,
		cache:      make(map[string]*Agent),
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				agent, err := r.Get("reviewer")
				if err != nil {
					t.Errorf("worker %d Get(reviewer): %v", worker, err)
					return
				}
				if agent.Name != "reviewer" {
					t.Errorf("worker %d got agent %q, want reviewer", worker, agent.Name)
					return
				}
			}
		}(i)
	}

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.SetPreferences(map[string]config.AgentPreference{
					"reviewer": {Model: fmt.Sprintf("test-model-%d-%d", worker, j)},
				})
			}
		}(i)
	}

	wg.Wait()
}

func TestRegistry_GetConcurrentColdBuiltin(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		r := &Registry{
			useBuiltin: true,
			cache:      make(map[string]*Agent),
		}

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				agent, err := r.Get("reviewer")
				if err != nil {
					t.Errorf("iteration %d worker %d Get(reviewer): %v", iter, worker, err)
					return
				}
				if agent.Name != "reviewer" {
					t.Errorf("iteration %d worker %d got agent %q, want reviewer", iter, worker, agent.Name)
				}
			}(i)
		}
		wg.Wait()
	}
}
