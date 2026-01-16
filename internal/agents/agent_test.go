package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromDir(t *testing.T) {
	// Create temp directory with agent files
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "test-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write agent.yaml
	agentYAML := `name: test-agent
description: "A test agent"
provider: anthropic
model: claude-sonnet-4-5
tools:
  enabled: [read, glob, grep]
shell:
  allow: ["git *"]
  auto_run: true
read:
  dirs: ["."]
max_turns: 10
mcp:
  - name: filesystem
`
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Write system.md
	systemMD := "You are a helpful assistant for {{git_repo}}."
	if err := os.WriteFile(filepath.Join(agentDir, "system.md"), []byte(systemMD), 0644); err != nil {
		t.Fatal(err)
	}

	// Load agent
	agent, err := LoadFromDir(agentDir, SourceUser)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}

	// Verify fields
	if agent.Name != "test-agent" {
		t.Errorf("Name = %q, want %q", agent.Name, "test-agent")
	}
	if agent.Description != "A test agent" {
		t.Errorf("Description = %q, want %q", agent.Description, "A test agent")
	}
	if agent.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", agent.Provider, "anthropic")
	}
	if agent.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want %q", agent.Model, "claude-sonnet-4-5")
	}
	if len(agent.Tools.Enabled) != 3 {
		t.Errorf("len(Tools.Enabled) = %d, want 3", len(agent.Tools.Enabled))
	}
	if agent.Shell.AutoRun != true {
		t.Error("Shell.AutoRun = false, want true")
	}
	if agent.MaxTurns != 10 {
		t.Errorf("MaxTurns = %d, want 10", agent.MaxTurns)
	}
	if agent.SystemPrompt != systemMD {
		t.Errorf("SystemPrompt = %q, want %q", agent.SystemPrompt, systemMD)
	}
	if agent.Source != SourceUser {
		t.Errorf("Source = %v, want %v", agent.Source, SourceUser)
	}
}

func TestLoadFromDir_MinimalConfig(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "minimal")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Minimal agent.yaml (no name, uses directory name)
	agentYAML := `description: "Minimal agent"`
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0644); err != nil {
		t.Fatal(err)
	}

	agent, err := LoadFromDir(agentDir, SourceLocal)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}

	// Name should be derived from directory
	if agent.Name != "minimal" {
		t.Errorf("Name = %q, want %q", agent.Name, "minimal")
	}
	if agent.SystemPrompt != "" {
		t.Errorf("SystemPrompt should be empty, got %q", agent.SystemPrompt)
	}
}

func TestAgent_GetEnabledTools(t *testing.T) {
	allTools := []string{"read", "write", "edit", "shell", "grep", "glob"}

	tests := []struct {
		name     string
		agent    Agent
		expected []string
	}{
		{
			name: "enabled list",
			agent: Agent{
				Name:  "test",
				Tools: ToolsConfig{Enabled: []string{"read", "glob"}},
			},
			expected: []string{"read", "glob"},
		},
		{
			name: "disabled list",
			agent: Agent{
				Name:  "test",
				Tools: ToolsConfig{Disabled: []string{"write", "shell"}},
			},
			expected: []string{"read", "edit", "grep", "glob"},
		},
		{
			name: "neither list",
			agent: Agent{
				Name: "test",
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.agent.GetEnabledTools(allTools)
			if len(got) != len(tt.expected) {
				t.Errorf("len(GetEnabledTools()) = %d, want %d", len(got), len(tt.expected))
				return
			}
			for i, tool := range got {
				if tool != tt.expected[i] {
					t.Errorf("GetEnabledTools()[%d] = %q, want %q", i, tool, tt.expected[i])
				}
			}
		})
	}
}

func TestAgent_Validate(t *testing.T) {
	tests := []struct {
		name    string
		agent   Agent
		wantErr bool
	}{
		{
			name:    "valid agent",
			agent:   Agent{Name: "test"},
			wantErr: false,
		},
		{
			name:    "missing name",
			agent:   Agent{},
			wantErr: true,
		},
		{
			name: "both enabled and disabled",
			agent: Agent{
				Name: "test",
				Tools: ToolsConfig{
					Enabled:  []string{"read"},
					Disabled: []string{"write"},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.agent.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAgent_GetMCPServerNames(t *testing.T) {
	agent := Agent{
		Name: "test",
		MCP: []MCPConfig{
			{Name: "filesystem"},
			{Name: "playwright", Command: "npx @playwright/mcp"},
		},
	}

	names := agent.GetMCPServerNames()
	if len(names) != 2 {
		t.Fatalf("len(GetMCPServerNames()) = %d, want 2", len(names))
	}
	if names[0] != "filesystem" {
		t.Errorf("names[0] = %q, want %q", names[0], "filesystem")
	}
	if names[1] != "playwright" {
		t.Errorf("names[1] = %q, want %q", names[1], "playwright")
	}
}
