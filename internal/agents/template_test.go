package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExpandTemplate(t *testing.T) {
	ctx := TemplateContext{
		Date:          "2026-01-16",
		DateTime:      "2026-01-16 14:30:00",
		Time:          "14:30",
		Year:          "2026",
		Cwd:           "/home/user/project",
		CwdName:       "project",
		Home:          "/home/user",
		User:          "testuser",
		GitBranch:     "main",
		GitRepo:       "term-llm",
		Files:         "main.go, utils.go",
		FileCount:     "2",
		OS:            "linux",
		Platform:      "chat",
		Provider:      "chatgpt",
		Model:         "gpt-5.4-medium",
		ProviderModel: "chatgpt:gpt-5.4-medium",
		ResourceDir:   "/home/user/.cache/term-llm/agents/artist",
		HandoverDir:   "/home/user/.local/share/term-llm/handover/project-abc123",
		HandoverPath:  "/home/user/.local/share/term-llm/handover/project-abc123/2026-01-16-amber-creek-bloom.md",
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
			name:     "platform variable",
			template: "Platform: {{platform}}",
			expected: "Platform: chat",
		},
		{
			name:     "provider variable",
			template: "Provider: {{provider}}",
			expected: "Provider: chatgpt",
		},
		{
			name:     "model variable",
			template: "Model: {{model}}",
			expected: "Model: gpt-5.4-medium",
		},
		{
			name:     "provider_model variable",
			template: "LLM: {{provider_model}}",
			expected: "LLM: chatgpt:gpt-5.4-medium",
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
			template: "{{date}} {{datetime}} {{time}} {{year}} {{cwd}} {{cwd_name}} {{home}} {{user}} {{git_branch}} {{git_repo}} {{files}} {{file_count}} {{os}} {{platform}} {{provider}} {{model}} {{provider_model}} {{resource_dir}} {{handover_dir}} {{handover_path}}",
			expected: "2026-01-16 2026-01-16 14:30:00 14:30 2026 /home/user/project project /home/user testuser main term-llm main.go, utils.go 2 linux chat chatgpt gpt-5.4-medium chatgpt:gpt-5.4-medium /home/user/.cache/term-llm/agents/artist /home/user/.local/share/term-llm/handover/project-abc123 /home/user/.local/share/term-llm/handover/project-abc123/2026-01-16-amber-creek-bloom.md",
		},
		{
			name:     "handover_dir variable",
			template: "Write to {{handover_dir}}/plan.md",
			expected: "Write to /home/user/.local/share/term-llm/handover/project-abc123/plan.md",
		},
		{
			name:     "handover_path variable",
			template: "Write to {{handover_path}}",
			expected: "Write to /home/user/.local/share/term-llm/handover/project-abc123/2026-01-16-amber-creek-bloom.md",
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

func TestNewTemplateContextForTemplate_SkipsGitWhenTemplateDoesNotUseGitFields(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git PATH shim test requires a POSIX shell")
	}

	shimDir := t.TempDir()
	logPath := filepath.Join(shimDir, "git.log")
	gitPath := filepath.Join(shimDir, "git")
	script := "#!/bin/sh\necho invoked >> \"" + logPath + "\"\nprintf 'feature/test\\n/tmp/fake-repo\\n'\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}

	origPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", shimDir+string(os.PathListSeparator)+origPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	defer os.Setenv("PATH", origPath)

	ctx := NewTemplateContextForTemplate("You are a helpful assistant. Today's date is {{date}}.")
	if ctx.GitBranch != "" {
		t.Fatalf("GitBranch = %q, want empty", ctx.GitBranch)
	}
	if ctx.GitRepo != "" {
		t.Fatalf("GitRepo = %q, want empty", ctx.GitRepo)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("expected git shim to not be invoked, stat err = %v", err)
	}
}

func TestNewTemplateContextForTemplate_CoalescesGitLookups(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git PATH shim test requires a POSIX shell")
	}

	shimDir := t.TempDir()
	logPath := filepath.Join(shimDir, "git.log")
	gitPath := filepath.Join(shimDir, "git")
	script := "#!/bin/sh\necho invoked >> \"" + logPath + "\"\nprintf 'feature/test\\n/tmp/fake-repo\\n'\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}

	origPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", shimDir+string(os.PathListSeparator)+origPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	defer os.Setenv("PATH", origPath)

	ctx := NewTemplateContextForTemplate("{{git_repo}}/{{git_branch}}")
	if ctx.GitBranch != "feature/test" {
		t.Fatalf("GitBranch = %q, want %q", ctx.GitBranch, "feature/test")
	}
	if ctx.GitRepo != "fake-repo" {
		t.Fatalf("GitRepo = %q, want %q", ctx.GitRepo, "fake-repo")
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read git log: %v", err)
	}
	invocations := strings.Count(strings.TrimSpace(string(logData)), "invoked")
	if invocations != 1 {
		t.Fatalf("git invoked %d times, want 1", invocations)
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

func TestTemplateContext_WithPlatform(t *testing.T) {
	ctx := TemplateContext{}

	ctx2 := ctx.WithPlatform("chat")
	if ctx2.Platform != "chat" {
		t.Errorf("Platform = %q, want %q", ctx2.Platform, "chat")
	}
	if ctx.Platform != "" {
		t.Errorf("Original Platform = %q, want empty", ctx.Platform)
	}
}

func TestTemplateContext_WithLLM(t *testing.T) {
	ctx := TemplateContext{}

	ctx2 := ctx.WithLLM("chatgpt", "gpt-5.4-medium")
	if ctx2.Provider != "chatgpt" {
		t.Errorf("Provider = %q, want %q", ctx2.Provider, "chatgpt")
	}
	if ctx2.Model != "gpt-5.4-medium" {
		t.Errorf("Model = %q, want %q", ctx2.Model, "gpt-5.4-medium")
	}
	if ctx2.ProviderModel != "chatgpt:gpt-5.4-medium" {
		t.Errorf("ProviderModel = %q, want %q", ctx2.ProviderModel, "chatgpt:gpt-5.4-medium")
	}
	if ctx.Provider != "" || ctx.Model != "" || ctx.ProviderModel != "" {
		t.Errorf("Original context mutated: %+v", ctx)
	}
}

func TestExpandTemplate_PlatformTokenUnchangedWhenUnavailable(t *testing.T) {
	result := ExpandTemplate("Platform={{platform}}", TemplateContext{})
	if result != "Platform={{platform}}" {
		t.Fatalf("ExpandTemplate() = %q, want %q", result, "Platform={{platform}}")
	}
}

func TestExpandTemplate_LLMTokensUnchangedWhenUnavailable(t *testing.T) {
	result := ExpandTemplate("Provider={{provider}} Model={{model}} LLM={{provider_model}}", TemplateContext{})
	if result != "Provider={{provider}} Model={{model}} LLM={{provider_model}}" {
		t.Fatalf("ExpandTemplate() = %q, want %q", result, "Provider={{provider}} Model={{model}} LLM={{provider_model}}")
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

func TestFindFallbackInstructions(t *testing.T) {
	// Create a temp directory structure: root/sub
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.MkdirAll(sub, 0o755)

	t.Run("no files found", func(t *testing.T) {
		result := findFallbackInstructions(sub, root)
		if result != "" {
			t.Errorf("expected empty, got %q", result)
		}
	})

	t.Run("CLAUDE.md in root", func(t *testing.T) {
		os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("claude instructions"), 0o644)
		defer os.Remove(filepath.Join(root, "CLAUDE.md"))

		result := findFallbackInstructions(sub, root)
		if result != "claude instructions" {
			t.Errorf("expected 'claude instructions', got %q", result)
		}
	})

	t.Run("CLAUDE.md in cwd takes precedence over root", func(t *testing.T) {
		os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("root claude"), 0o644)
		os.WriteFile(filepath.Join(sub, "CLAUDE.md"), []byte("sub claude"), 0o644)
		defer os.Remove(filepath.Join(root, "CLAUDE.md"))
		defer os.Remove(filepath.Join(sub, "CLAUDE.md"))

		result := findFallbackInstructions(sub, root)
		if result != "sub claude" {
			t.Errorf("expected 'sub claude', got %q", result)
		}
	})
}

func TestLoadProjectInstructions_Hierarchy(t *testing.T) {
	// Create a temp repo with a real git init
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.MkdirAll(sub, 0o755)

	// Initialize a real git repo so findGitRoot works
	cmd := exec.Command("git", "init", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	// Save and restore cwd
	origCwd, _ := os.Getwd()
	defer os.Chdir(origCwd)

	t.Run("hierarchical AGENTS.md from root to cwd", func(t *testing.T) {
		os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root agents"), 0o644)
		os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("sub agents"), 0o644)
		os.Chdir(sub)

		result := loadProjectInstructions()
		if !strings.Contains(result, "root agents") {
			t.Errorf("expected root AGENTS.md content, got %q", result)
		}
		if !strings.Contains(result, "sub agents") {
			t.Errorf("expected sub AGENTS.md content, got %q", result)
		}
		// root should come before sub
		rootIdx := strings.Index(result, "root agents")
		subIdx := strings.Index(result, "sub agents")
		if rootIdx > subIdx {
			t.Error("root AGENTS.md should come before sub AGENTS.md")
		}

		os.Remove(filepath.Join(root, "AGENTS.md"))
		os.Remove(filepath.Join(sub, "AGENTS.md"))
	})

	t.Run("AGENTS.override.md takes precedence at same level", func(t *testing.T) {
		os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root agents"), 0o644)
		os.WriteFile(filepath.Join(root, "AGENTS.override.md"), []byte("root override"), 0o644)
		os.Chdir(root)

		result := loadProjectInstructions()
		if !strings.Contains(result, "root override") {
			t.Errorf("expected override content, got %q", result)
		}
		if strings.Contains(result, "root agents") {
			t.Errorf("AGENTS.md should be skipped when override exists, got %q", result)
		}

		os.Remove(filepath.Join(root, "AGENTS.md"))
		os.Remove(filepath.Join(root, "AGENTS.override.md"))
	})

	t.Run("fallback to CLAUDE.md when no AGENTS.md", func(t *testing.T) {
		os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("claude instructions"), 0o644)
		os.Chdir(root)

		result := loadProjectInstructions()
		if !strings.Contains(result, "claude instructions") {
			t.Errorf("expected CLAUDE.md fallback, got %q", result)
		}

		os.Remove(filepath.Join(root, "CLAUDE.md"))
	})

	t.Run("AGENTS.md wins over CLAUDE.md", func(t *testing.T) {
		os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("agents instructions"), 0o644)
		os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("claude instructions"), 0o644)
		os.Chdir(root)

		result := loadProjectInstructions()
		if !strings.Contains(result, "agents instructions") {
			t.Errorf("expected AGENTS.md content, got %q", result)
		}
		if strings.Contains(result, "claude instructions") {
			t.Errorf("CLAUDE.md should not be loaded when AGENTS.md exists, got %q", result)
		}

		os.Remove(filepath.Join(root, "AGENTS.md"))
		os.Remove(filepath.Join(root, "CLAUDE.md"))
	})
}
