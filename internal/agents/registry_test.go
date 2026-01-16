package agents

import (
	"os"
	"path/filepath"
	"testing"
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

	// Check that agents are sorted
	for i := 1; i < len(agents); i++ {
		if agents[i-1].Name > agents[i].Name {
			t.Errorf("agents not sorted: %s > %s", agents[i-1].Name, agents[i].Name)
		}
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
