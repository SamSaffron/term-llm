package agents

import (
	"strings"
	"testing"
	"time"
)

func TestExpandTemplate(t *testing.T) {
	ctx := TemplateContext{
		Date:        "2026-01-16",
		DateTime:    "2026-01-16 14:30:00",
		Time:        "14:30",
		Year:        "2026",
		Cwd:         "/home/user/project",
		CwdName:     "project",
		Home:        "/home/user",
		User:        "testuser",
		GitBranch:   "main",
		GitRepo:     "term-llm",
		Files:       "main.go, utils.go",
		FileCount:   "2",
		OS:          "linux",
		ResourceDir: "/home/user/.cache/term-llm/agents/artist",
		Platform:    "telegram",
	}

	tests := []struct {
		name     string
		template string
		expected string
	}{
		{
			name:     "simple variable",
			template: "Today is {{date}}",
			expected: "Today is 2026-01-16",
		},
		{
			name:     "multiple variables",
			template: "{{user}} is working on {{git_repo}} (branch: {{git_branch}})",
			expected: "testuser is working on term-llm (branch: main)",
		},
		{
			name:     "no variables",
			template: "Just plain text",
			expected: "Just plain text",
		},
		{
			name:     "unknown variable",
			template: "Hello {{unknown}}",
			expected: "Hello {{unknown}}",
		},
		{
			name:     "all variables",
			template: "{{date}} {{datetime}} {{time}} {{year}} {{cwd}} {{cwd_name}} {{home}} {{user}} {{git_branch}} {{git_repo}} {{files}} {{file_count}} {{os}} {{resource_dir}} {{platform}}",
			expected: "2026-01-16 2026-01-16 14:30:00 14:30 2026 /home/user/project project /home/user testuser main term-llm main.go, utils.go 2 linux /home/user/.cache/term-llm/agents/artist telegram",
		},
		{
			name:     "platform variable",
			template: "Running on {{platform}}",
			expected: "Running on telegram",
		},

		{
			name:     "resource_dir variable",
			template: "Read styles at {{resource_dir}}/styles.md",
			expected: "Read styles at /home/user/.cache/term-llm/agents/artist/styles.md",
		},
		{
			name:     "adjacent variables",
			template: "{{git_repo}}/{{git_branch}}",
			expected: "term-llm/main",
		},
		{
			name:     "empty string for empty values",
			template: "branch: {{git_branch}}",
			expected: "branch: main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExpandTemplate(tt.template, ctx)
			if result != tt.expected {
				t.Errorf("ExpandTemplate() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestNewTemplateContext(t *testing.T) {
	ctx := NewTemplateContext()

	// Check that date-related fields are populated
	if ctx.Date == "" {
		t.Error("Date should not be empty")
	}
	if ctx.DateTime == "" {
		t.Error("DateTime should not be empty")
	}
	if ctx.Time == "" {
		t.Error("Time should not be empty")
	}
	if ctx.Year == "" {
		t.Error("Year should not be empty")
	}

	// Verify date format
	_, err := time.Parse("2006-01-02", ctx.Date)
	if err != nil {
		t.Errorf("Date format invalid: %v", err)
	}

	// Check OS
	if ctx.OS == "" {
		t.Error("OS should not be empty")
	}

	// Check that cwd is populated (should be valid in test)
	if ctx.Cwd == "" {
		t.Error("Cwd should not be empty")
	}
}

func TestTemplateContext_WithPlatform(t *testing.T) {
	ctx := TemplateContext{}

	// Set platform
	ctx2 := ctx.WithPlatform("telegram")
	if ctx2.Platform != "telegram" {
		t.Errorf("Platform = %q, want %q", ctx2.Platform, "telegram")
	}

	// Original unchanged
	if ctx.Platform != "" {
		t.Errorf("Original Platform should be empty, got %q", ctx.Platform)
	}

	// Empty platform — variable is left unexpanded (deferred for per-session substitution)
	result := ExpandTemplate("platform: {{platform}}", TemplateContext{})
	if result != "platform: {{platform}}" {
		t.Errorf("ExpandTemplate with empty platform = %q, want %q", result, "platform: {{platform}}")
	}
}

func TestTemplateContext_WithFiles(t *testing.T) {
	ctx := TemplateContext{
		Date: "2026-01-16",
	}

	// With files
	ctx2 := ctx.WithFiles([]string{"/path/to/main.go", "/path/to/utils.go"})
	if ctx2.FileCount != "2" {
		t.Errorf("FileCount = %q, want %q", ctx2.FileCount, "2")
	}
	if !strings.Contains(ctx2.Files, "main.go") {
		t.Errorf("Files should contain main.go, got %q", ctx2.Files)
	}
	if !strings.Contains(ctx2.Files, "utils.go") {
		t.Errorf("Files should contain utils.go, got %q", ctx2.Files)
	}

	// Without files
	ctx3 := ctx.WithFiles(nil)
	if ctx3.FileCount != "0" {
		t.Errorf("FileCount = %q, want %q", ctx3.FileCount, "0")
	}
	if ctx3.Files != "" {
		t.Errorf("Files = %q, want empty string", ctx3.Files)
	}
}

func TestItoa(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, "0"},
		{1, "1"},
		{123, "123"},
		{-5, "-5"},
		{1000000, "1000000"},
	}

	for _, tt := range tests {
		result := itoa(tt.input)
		if result != tt.expected {
			t.Errorf("itoa(%d) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
