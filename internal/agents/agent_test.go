package agents

import (
	"os"
	"path/filepath"
	"strings"
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
		{
			name: "valid project_instructions empty",
			agent: Agent{
				Name:                "test",
				ProjectInstructions: "",
			},
			wantErr: false,
		},
		{
			name: "valid project_instructions auto",
			agent: Agent{
				Name:                "test",
				ProjectInstructions: "auto",
			},
			wantErr: false,
		},
		{
			name: "valid project_instructions true",
			agent: Agent{
				Name:                "test",
				ProjectInstructions: "true",
			},
			wantErr: false,
		},
		{
			name: "valid project_instructions false",
			agent: Agent{
				Name:                "test",
				ProjectInstructions: "false",
			},
			wantErr: false,
		},
		{
			name: "invalid project_instructions",
			agent: Agent{
				Name:                "test",
				ProjectInstructions: "yes",
			},
			wantErr: true,
		},
		{
			name: "invalid project_instructions typo",
			agent: Agent{
				Name:                "test",
				ProjectInstructions: "tru",
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

func TestIsAgentPath(t *testing.T) {
	tests := []struct {
		value    string
		expected bool
	}{
		{"reviewer", false},
		{"commit-message", false},
		{"my_agent", false},
		{"/abs/path/agent", true},
		{"./relative/agent", true},
		{"../parent/agent", true},
		{"path/to/agent", true},
		{"~/.config/agents/foo", true},
		// Windows-style paths (backslashes, drive letters)
		{`.\\rel\\agent`, true},
		{`C:\agents\foo`, true},
		{`D:\Users\me\agents\custom`, true},
		{`c:\lowercase-drive`, true},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got := IsAgentPath(tt.value)
			if got != tt.expected {
				t.Errorf("IsAgentPath(%q) = %v, want %v", tt.value, got, tt.expected)
			}
		})
	}
}

func TestLoadFromPath(t *testing.T) {
	// Create temp agent directory
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "test-path-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}

	agentYAML := `name: path-agent
description: "Agent loaded from path"
max_turns: 5
`
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0644); err != nil {
		t.Fatal(err)
	}

	systemMD := "You are a path-loaded agent."
	if err := os.WriteFile(filepath.Join(agentDir, "system.md"), []byte(systemMD), 0644); err != nil {
		t.Fatal(err)
	}

	agent, err := LoadFromPath(agentDir)
	if err != nil {
		t.Fatalf("LoadFromPath: %v", err)
	}

	if agent.Name != "path-agent" {
		t.Errorf("Name = %q, want %q", agent.Name, "path-agent")
	}
	if agent.Description != "Agent loaded from path" {
		t.Errorf("Description = %q, want %q", agent.Description, "Agent loaded from path")
	}
	if agent.MaxTurns != 5 {
		t.Errorf("MaxTurns = %d, want 5", agent.MaxTurns)
	}
	if agent.SystemPrompt != systemMD {
		t.Errorf("SystemPrompt = %q, want %q", agent.SystemPrompt, systemMD)
	}
	if agent.Source != SourceLocal {
		t.Errorf("Source = %v, want %v", agent.Source, SourceLocal)
	}
	// SourcePath should be an absolute path
	if !filepath.IsAbs(agent.SourcePath) {
		t.Errorf("SourcePath should be absolute, got %q", agent.SourcePath)
	}
}

func TestLoadFromPath_Relative(t *testing.T) {
	// Create a temp dir and an agent inside it
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "rel-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}

	agentYAML := `name: rel-agent
description: "Relative path agent"
`
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Change to tmpDir so we can use a relative path
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	agent, err := LoadFromPath("./rel-agent")
	if err != nil {
		t.Fatalf("LoadFromPath with relative path: %v", err)
	}

	if agent.Name != "rel-agent" {
		t.Errorf("Name = %q, want %q", agent.Name, "rel-agent")
	}
	if !filepath.IsAbs(agent.SourcePath) {
		t.Errorf("SourcePath should be absolute even for relative input, got %q", agent.SourcePath)
	}
}

func TestLoadFromPath_MissingAgentYaml(t *testing.T) {
	// Directory exists but has no agent.yaml
	tmpDir := t.TempDir()

	_, err := LoadFromPath(tmpDir)
	if err == nil {
		t.Fatal("expected error for directory without agent.yaml, got nil")
	}
	if !strings.Contains(err.Error(), "missing agent.yaml") {
		t.Errorf("error should mention missing agent.yaml, got: %v", err)
	}
}

func TestLoadFromPath_NonexistentDir(t *testing.T) {
	_, err := LoadFromPath("/nonexistent/path/to/agent")
	if err == nil {
		t.Fatal("expected error for nonexistent path, got nil")
	}
	if !strings.Contains(err.Error(), "missing agent.yaml") {
		t.Errorf("error should mention missing agent.yaml, got: %v", err)
	}
}

func TestLoadFromPath_DerivesNameFromDir(t *testing.T) {
	// agent.yaml without a name field — name derived from directory
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "my-custom-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}

	agentYAML := `description: "No name field"`
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0644); err != nil {
		t.Fatal(err)
	}

	agent, err := LoadFromPath(agentDir)
	if err != nil {
		t.Fatalf("LoadFromPath: %v", err)
	}

	if agent.Name != "my-custom-agent" {
		t.Errorf("Name = %q, want %q (derived from directory)", agent.Name, "my-custom-agent")
	}
}

func TestAgent_ShouldLoadProjectInstructions(t *testing.T) {
	tests := []struct {
		name     string
		agent    Agent
		expected bool
	}{
		{
			name: "auto with coding tools (write_file)",
			agent: Agent{
				Name:  "developer",
				Tools: ToolsConfig{Enabled: []string{"read_file", "write_file"}},
			},
			expected: true,
		},
		{
			name: "auto with coding tools (edit_file)",
			agent: Agent{
				Name:  "editor",
				Tools: ToolsConfig{Enabled: []string{"read_file", "edit_file"}},
			},
			expected: true,
		},
		{
			name: "auto with coding tools (shell)",
			agent: Agent{
				Name:  "shell",
				Tools: ToolsConfig{Enabled: []string{"shell"}},
			},
			expected: true,
		},
		{
			name: "auto without coding tools",
			agent: Agent{
				Name:  "researcher",
				Tools: ToolsConfig{Enabled: []string{"read_file", "glob", "grep"}},
			},
			expected: false,
		},
		{
			name: "explicit true without coding tools",
			agent: Agent{
				Name:                "custom",
				ProjectInstructions: "true",
				Tools:               ToolsConfig{Enabled: []string{"read_file"}},
			},
			expected: true,
		},
		{
			name: "explicit false with coding tools",
			agent: Agent{
				Name:                "artist",
				ProjectInstructions: "false",
				Tools:               ToolsConfig{Enabled: []string{"shell", "image_generate"}},
			},
			expected: false,
		},
		{
			name: "empty tools (auto defaults to true - assumes default tool set)",
			agent: Agent{
				Name: "empty",
			},
			expected: true,
		},
		{
			name: "disabled list - coding tools not disabled",
			agent: Agent{
				Name:  "searcher",
				Tools: ToolsConfig{Disabled: []string{"image_generate", "web_search"}},
			},
			expected: true,
		},
		{
			name: "disabled list - all coding tools disabled",
			agent: Agent{
				Name:  "readonly",
				Tools: ToolsConfig{Disabled: []string{"write_file", "edit_file", "shell"}},
			},
			expected: false,
		},
		{
			name: "disabled list - some coding tools disabled",
			agent: Agent{
				Name:  "limited",
				Tools: ToolsConfig{Disabled: []string{"shell", "image_generate"}},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.agent.ShouldLoadProjectInstructions()
			if got != tt.expected {
				t.Errorf("ShouldLoadProjectInstructions() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestValidateCustomTools(t *testing.T) {
	tests := []struct {
		name    string
		defs    []CustomToolDef
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid single tool",
			defs: []CustomToolDef{
				{Name: "job_status", Description: "Get status", Script: "scripts/job.sh"},
			},
			wantErr: false,
		},
		{
			name: "valid with input schema",
			defs: []CustomToolDef{
				{
					Name:        "job_run",
					Description: "Run a job",
					Script:      "scripts/job-run.sh",
					Input: map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "valid with subdirectory script path",
			defs: []CustomToolDef{
				{Name: "my_tool", Description: "x", Script: "scripts/subdir/tool.sh"},
			},
			wantErr: false,
		},
		{
			name:    "empty name",
			defs:    []CustomToolDef{{Name: "", Description: "x", Script: "x.sh"}},
			wantErr: true,
			errMsg:  "name is required",
		},
		{
			name:    "uppercase in name",
			defs:    []CustomToolDef{{Name: "MyTool", Description: "x", Script: "x.sh"}},
			wantErr: true,
			errMsg:  "must match",
		},
		{
			name:    "hyphen in name",
			defs:    []CustomToolDef{{Name: "my-tool", Description: "x", Script: "x.sh"}},
			wantErr: true,
			errMsg:  "must match",
		},
		{
			name:    "name starts with digit",
			defs:    []CustomToolDef{{Name: "1tool", Description: "x", Script: "x.sh"}},
			wantErr: true,
			errMsg:  "must match",
		},
		{
			name: "duplicate names",
			defs: []CustomToolDef{
				{Name: "my_tool", Description: "a", Script: "a.sh"},
				{Name: "my_tool", Description: "b", Script: "b.sh"},
			},
			wantErr: true,
			errMsg:  "duplicate",
		},
		{
			name:    "missing description",
			defs:    []CustomToolDef{{Name: "my_tool", Description: "", Script: "x.sh"}},
			wantErr: true,
			errMsg:  "description is required",
		},
		{
			name:    "missing script",
			defs:    []CustomToolDef{{Name: "my_tool", Description: "x", Script: ""}},
			wantErr: true,
			errMsg:  "script is required",
		},
		{
			name:    "absolute script path",
			defs:    []CustomToolDef{{Name: "my_tool", Description: "x", Script: "/etc/evil.sh"}},
			wantErr: true,
			errMsg:  "relative path",
		},
		{
			name: "input schema wrong type",
			defs: []CustomToolDef{
				{
					Name:        "my_tool",
					Description: "x",
					Script:      "x.sh",
					Input:       map[string]interface{}{"type": "string"},
				},
			},
			wantErr: true,
			errMsg:  `"type": "object"`,
		},
		{
			name: "input schema missing type",
			defs: []CustomToolDef{
				{
					Name:        "my_tool",
					Description: "x",
					Script:      "x.sh",
					Input:       map[string]interface{}{"properties": map[string]interface{}{}},
				},
			},
			wantErr: true,
			errMsg:  `"type": "object"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCustomTools(tt.defs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errMsg)
				}
				if tt.errMsg != "" {
					if !containsSubstr(err.Error(), tt.errMsg) {
						t.Errorf("expected error to contain %q, got %q", tt.errMsg, err.Error())
					}
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func containsSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
