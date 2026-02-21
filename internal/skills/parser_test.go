package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSkillMDContent_Valid(t *testing.T) {
	content := `---
name: test-skill
description: "A test skill for unit testing"
license: MIT
compatibility: "Go 1.20+"
allowed-tools: read grep glob
metadata:
  author: test
  version: "1.0"
---

# Test Skill

This is the body of the skill.

## Guidelines

- Do something useful
`

	skill, err := ParseSkillMDContent(content, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if skill.Name != "test-skill" {
		t.Errorf("expected name 'test-skill', got %q", skill.Name)
	}

	if skill.Description != "A test skill for unit testing" {
		t.Errorf("unexpected description: %q", skill.Description)
	}

	if skill.License != "MIT" {
		t.Errorf("expected license 'MIT', got %q", skill.License)
	}

	if skill.Compatibility != "Go 1.20+" {
		t.Errorf("expected compatibility 'Go 1.20+', got %q", skill.Compatibility)
	}

	expectedTools := []string{"read", "grep", "glob"}
	if len(skill.AllowedTools) != len(expectedTools) {
		t.Errorf("expected %d allowed tools, got %d", len(expectedTools), len(skill.AllowedTools))
	} else {
		for i, tool := range expectedTools {
			if skill.AllowedTools[i] != tool {
				t.Errorf("expected tool %q at index %d, got %q", tool, i, skill.AllowedTools[i])
			}
		}
	}

	if skill.Metadata["author"] != "test" {
		t.Errorf("expected metadata author 'test', got %q", skill.Metadata["author"])
	}

	if skill.Metadata["version"] != "1.0" {
		t.Errorf("expected metadata version '1.0', got %q", skill.Metadata["version"])
	}

	if !skill.IsLoaded() {
		t.Error("expected skill to be loaded")
	}

	if skill.Body == "" {
		t.Error("expected body to be loaded")
	}
}

func TestParseSkillMDContent_MetadataOnly(t *testing.T) {
	content := `---
name: metadata-only
description: "Testing metadata-only loading"
---

# Body content that should not be loaded
`

	skill, err := ParseSkillMDContent(content, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if skill.Name != "metadata-only" {
		t.Errorf("expected name 'metadata-only', got %q", skill.Name)
	}

	if skill.IsLoaded() {
		t.Error("expected skill to not be fully loaded")
	}

	if skill.Body != "" {
		t.Error("expected body to be empty for metadata-only load")
	}
}

func TestParseSkillMDContent_AllowedToolsFormats(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []string
	}{
		{
			name: "space-delimited (spec)",
			content: `---
name: test
description: test
allowed-tools: read grep glob shell
---
`,
			expected: []string{"read", "grep", "glob", "shell"},
		},
		{
			name: "comma-delimited (Claude Code)",
			content: `---
name: test
description: test
allowed-tools: read, grep, glob, shell
---
`,
			expected: []string{"read", "grep", "glob", "shell"},
		},
		{
			name: "yaml list",
			content: `---
name: test
description: test
allowed-tools:
  - read
  - grep
  - glob
---
`,
			expected: []string{"read", "grep", "glob"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skill, err := ParseSkillMDContent(tt.content, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(skill.AllowedTools) != len(tt.expected) {
				t.Errorf("expected %d tools, got %d: %v", len(tt.expected), len(skill.AllowedTools), skill.AllowedTools)
				return
			}

			for i, tool := range tt.expected {
				if skill.AllowedTools[i] != tool {
					t.Errorf("expected tool %q at index %d, got %q", tool, i, skill.AllowedTools[i])
				}
			}
		})
	}
}

func TestParseSkillMDContent_UnknownFields(t *testing.T) {
	content := `---
name: extras-test
description: test with extra fields
model: claude-4
context: full
user-invocable: true
hooks:
  pre-run: echo hello
---
`

	skill, err := ParseSkillMDContent(content, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if skill.Extras == nil {
		t.Fatal("expected extras to be populated")
	}

	if skill.Extras["model"] != "claude-4" {
		t.Errorf("expected model 'claude-4' in extras, got %v", skill.Extras["model"])
	}

	if skill.Extras["context"] != "full" {
		t.Errorf("expected context 'full' in extras, got %v", skill.Extras["context"])
	}

	if skill.Extras["user-invocable"] != true {
		t.Errorf("expected user-invocable true in extras, got %v", skill.Extras["user-invocable"])
	}

	// Hooks should be preserved as a map
	if skill.Extras["hooks"] == nil {
		t.Error("expected hooks in extras")
	}
}

func TestParseSkillMDContent_NoFrontmatter(t *testing.T) {
	content := `# No Frontmatter

This is just markdown without frontmatter.
`

	_, err := ParseSkillMDContent(content, false)
	if err == nil {
		t.Error("expected error for missing frontmatter")
	}
}

func TestParseSkillMDContent_EmptyFrontmatter(t *testing.T) {
	content := `---
---

# Body
`

	// Empty frontmatter is an error
	_, err := ParseSkillMDContent(content, false)
	if err == nil {
		t.Error("expected error for empty frontmatter")
	}
}

func TestSkillValidate(t *testing.T) {
	tests := []struct {
		name    string
		skill   Skill
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid skill",
			skill: Skill{
				Name:        "valid-skill",
				Description: "A valid skill",
			},
			wantErr: false,
		},
		{
			name: "empty name",
			skill: Skill{
				Description: "Missing name",
			},
			wantErr: true,
			errMsg:  "name is required",
		},
		{
			name: "name too long",
			skill: Skill{
				Name:        "this-is-a-very-long-skill-name-that-exceeds-the-maximum-allowed-length-of-64-characters",
				Description: "Too long",
			},
			wantErr: true,
			errMsg:  "1-64 characters",
		},
		{
			name: "invalid name characters",
			skill: Skill{
				Name:        "Invalid_Name",
				Description: "Has underscore",
			},
			wantErr: true,
			errMsg:  "lowercase letters",
		},
		{
			name: "leading hyphen",
			skill: Skill{
				Name:        "-leading",
				Description: "Leading hyphen",
			},
			wantErr: true,
			errMsg:  "lowercase letters",
		},
		{
			name: "trailing hyphen",
			skill: Skill{
				Name:        "trailing-",
				Description: "Trailing hyphen",
			},
			wantErr: true,
			errMsg:  "lowercase letters",
		},
		{
			name: "consecutive hyphens",
			skill: Skill{
				Name:        "double--hyphen",
				Description: "Has consecutive hyphens",
			},
			wantErr: true,
			errMsg:  "consecutive hyphens",
		},
		{
			name: "empty description",
			skill: Skill{
				Name: "no-description",
			},
			wantErr: true,
			errMsg:  "description is required",
		},
		{
			name: "description too long",
			skill: Skill{
				Name:        "long-desc",
				Description: string(make([]byte, 1025)),
			},
			wantErr: true,
			errMsg:  "1-1024 characters",
		},
		{
			name: "compatibility too long",
			skill: Skill{
				Name:          "long-compat",
				Description:   "Valid",
				Compatibility: string(make([]byte, 501)),
			},
			wantErr: true,
			errMsg:  "<= 500 characters",
		},
		{
			name: "single char name",
			skill: Skill{
				Name:        "a",
				Description: "Single char name is valid",
			},
			wantErr: false,
		},
		{
			name: "number in name",
			skill: Skill{
				Name:        "skill-v2",
				Description: "Name with numbers",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.skill.Validate()
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got nil")
				} else if tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestLoadFromDir(t *testing.T) {
	// Create a temporary directory
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "test-skill")

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	// Create SKILL.md
	content := `---
name: test-skill
description: "A test skill"
---

# Test Skill Instructions
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	// Create references directory with a file
	refsDir := filepath.Join(skillDir, "references")
	if err := os.MkdirAll(refsDir, 0755); err != nil {
		t.Fatalf("failed to create references dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(refsDir, "guide.md"), []byte("# Guide"), 0644); err != nil {
		t.Fatalf("failed to write reference file: %v", err)
	}

	// Load with full content
	skill, err := LoadFromDir(skillDir, SourceLocal, true)
	if err != nil {
		t.Fatalf("failed to load skill: %v", err)
	}

	if skill.Name != "test-skill" {
		t.Errorf("expected name 'test-skill', got %q", skill.Name)
	}

	if skill.Source != SourceLocal {
		t.Errorf("expected source SourceLocal, got %v", skill.Source)
	}

	if skill.SourcePath != skillDir {
		t.Errorf("expected source path %q, got %q", skillDir, skill.SourcePath)
	}

	if len(skill.References) != 1 || skill.References[0] != "guide.md" {
		t.Errorf("expected references [guide.md], got %v", skill.References)
	}
}

func TestLoadFromDir_NameMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "actual-name")

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	// Create SKILL.md with mismatched name
	content := `---
name: different-name
description: "Name doesn't match directory"
---
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	_, err := LoadFromDir(skillDir, SourceLocal, false)
	if err == nil {
		t.Error("expected error for name mismatch")
	}
	if !contains(err.Error(), "must match directory") {
		t.Errorf("expected 'must match directory' error, got: %v", err)
	}
}

func TestParseSkillMDContent_Tools(t *testing.T) {
	content := `---
name: google-maps
description: "Google Maps skill with bundled tools"
tools:
  - name: maps_travel_time
    description: "Get traffic-aware travel time between two places"
    script: scripts/travel-time.sh
    timeout_seconds: 15
    input:
      type: object
      properties:
        origin:
          type: string
          description: "Origin address or lat,lng"
        destination:
          type: string
          description: "Destination address or lat,lng"
        mode:
          type: string
          description: "Travel mode: DRIVE, WALK, BICYCLE, TRANSIT"
      required: [origin, destination]
  - name: maps_places_search
    description: "Text search for places"
    script: scripts/places-search.sh
    input:
      type: object
      properties:
        query:
          type: string
      required: [query]
---

# Google Maps Skill
`

	skill, err := ParseSkillMDContent(content, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !skill.HasTools() {
		t.Fatal("expected skill to have tools")
	}
	if len(skill.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(skill.Tools))
	}

	tt := skill.Tools[0]
	if tt.Name != "maps_travel_time" {
		t.Errorf("expected tool name 'maps_travel_time', got %q", tt.Name)
	}
	if tt.Description != "Get traffic-aware travel time between two places" {
		t.Errorf("unexpected description: %q", tt.Description)
	}
	if tt.Script != "scripts/travel-time.sh" {
		t.Errorf("expected script 'scripts/travel-time.sh', got %q", tt.Script)
	}
	if tt.TimeoutSeconds != 15 {
		t.Errorf("expected timeout 15, got %d", tt.TimeoutSeconds)
	}
	if tt.Input == nil {
		t.Fatal("expected input schema to be set")
	}
	if tt.Input["type"] != "object" {
		t.Errorf("expected input type 'object', got %v", tt.Input["type"])
	}

	tt2 := skill.Tools[1]
	if tt2.Name != "maps_places_search" {
		t.Errorf("expected tool name 'maps_places_search', got %q", tt2.Name)
	}

	// Extras should not contain "tools" key
	if _, ok := skill.Extras["tools"]; ok {
		t.Error("'tools' should not appear in Extras")
	}
}

func TestParseSkillMDContent_NoTools(t *testing.T) {
	content := `---
name: simple-skill
description: "A skill with no tools"
---

Body here.
`
	skill, err := ParseSkillMDContent(content, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.HasTools() {
		t.Error("expected skill to have no tools")
	}
}

func TestIsSkillDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a skill directory
	skillDir := filepath.Join(tmpDir, "my-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\n---\n"), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	// Create an empty directory
	emptyDir := filepath.Join(tmpDir, "empty")
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatalf("failed to create empty dir: %v", err)
	}

	if !IsSkillDir(skillDir) {
		t.Error("expected IsSkillDir to return true for skill directory")
	}

	if IsSkillDir(emptyDir) {
		t.Error("expected IsSkillDir to return false for empty directory")
	}

	if IsSkillDir(filepath.Join(tmpDir, "nonexistent")) {
		t.Error("expected IsSkillDir to return false for nonexistent directory")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
