package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadProjectApprovals_EmptyRoot(t *testing.T) {
	pa, err := LoadProjectApprovals("")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if pa != nil {
		t.Error("expected nil for empty root")
	}
}

func TestLoadProjectApprovals_NewProject(t *testing.T) {
	// Use a temp directory as the "repo root"
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Override XDG_CONFIG_HOME to use temp directory for storage
	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pa == nil {
		t.Fatal("expected non-nil ProjectApprovals")
	}

	// Check defaults
	if pa.IsReadApproved() {
		t.Error("new project should not have read approved")
	}
	if pa.IsWriteApproved() {
		t.Error("new project should not have write approved")
	}
	if pa.RepoRoot != tempDir {
		t.Errorf("expected RepoRoot=%s, got %s", tempDir, pa.RepoRoot)
	}
	if pa.RepoName != filepath.Base(tempDir) {
		t.Errorf("expected RepoName=%s, got %s", filepath.Base(tempDir), pa.RepoName)
	}
}

func TestProjectApprovals_ReadApproval(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Approve read
	if err := pa.ApproveRead(); err != nil {
		t.Fatalf("ApproveRead failed: %v", err)
	}

	if !pa.IsReadApproved() {
		t.Error("expected read to be approved")
	}
	if pa.IsWriteApproved() {
		t.Error("write should not be approved")
	}

	// Reload and verify persistence
	pa2, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if !pa2.IsReadApproved() {
		t.Error("read approval should persist after reload")
	}
}

func TestProjectApprovals_WriteApproval(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Approve write
	if err := pa.ApproveWrite(); err != nil {
		t.Fatalf("ApproveWrite failed: %v", err)
	}

	if !pa.IsWriteApproved() {
		t.Error("expected write to be approved")
	}
	if pa.IsReadApproved() {
		t.Error("read should not be approved (only write was approved)")
	}

	// Reload and verify persistence
	pa2, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if !pa2.IsWriteApproved() {
		t.Error("write approval should persist after reload")
	}
}

func TestProjectApprovals_IsPathApproved(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	testFile := filepath.Join(tempDir, "src", "main.go")

	// Initially not approved
	if pa.IsPathApproved(testFile, false) {
		t.Error("path should not be approved initially")
	}

	// Approve read for entire repo
	if err := pa.ApproveRead(); err != nil {
		t.Fatalf("ApproveRead failed: %v", err)
	}

	// Now should be approved for read
	if !pa.IsPathApproved(testFile, false) {
		t.Error("path should be approved for read after repo approval")
	}

	// But not for write
	if pa.IsPathApproved(testFile, true) {
		t.Error("path should not be approved for write (only read was approved)")
	}

	// Approve write
	if err := pa.ApproveWrite(); err != nil {
		t.Fatalf("ApproveWrite failed: %v", err)
	}

	// Now should be approved for write too
	if !pa.IsPathApproved(testFile, true) {
		t.Error("path should be approved for write after write approval")
	}
}

func TestProjectApprovals_IsPathApproved_IndividualPaths(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Approve a specific directory
	srcDir := filepath.Join(tempDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	mainFile := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main"), 0644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := pa.ApprovePath(srcDir); err != nil {
		t.Fatalf("ApprovePath failed: %v", err)
	}

	// File in approved directory should be approved
	if !pa.IsPathApproved(mainFile, false) {
		t.Error("file in approved directory should be approved")
	}

	// File outside approved directory should not be approved
	if pa.IsPathApproved(filepath.Join(tempDir, "other", "file.go"), false) {
		t.Error("file outside approved directory should not be approved")
	}

	// Verify persistence
	pa2, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if !pa2.IsPathApproved(mainFile, false) {
		t.Error("path approval should persist after reload")
	}
}

func TestProjectApprovals_IsPathApproved_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test not supported on Windows")
	}

	repoRoot := t.TempDir()
	configDir := t.TempDir()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	approvedDir := filepath.Join(repoRoot, "approved")
	outsideDir := t.TempDir()
	if err := os.MkdirAll(approvedDir, 0755); err != nil {
		t.Fatalf("mkdir approved: %v", err)
	}
	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(approvedDir, "secret-link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	pa, err := LoadProjectApprovals(repoRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := pa.ApprovePath(approvedDir); err != nil {
		t.Fatalf("ApprovePath failed: %v", err)
	}
	if pa.IsPathApproved(link, false) {
		t.Fatal("symlink escape should not inherit project path approval")
	}
}

func TestProjectApprovals_ShellPatterns(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Initially no patterns approved
	if pa.IsShellPatternApproved("go test ./...") {
		t.Error("command should not be approved initially")
	}

	// Approve a pattern
	if err := pa.ApproveShellPattern("go test *"); err != nil {
		t.Fatalf("ApproveShellPattern failed: %v", err)
	}

	// Matching command should be approved
	if !pa.IsShellPatternApproved("go test ./...") {
		t.Error("matching command should be approved")
	}

	// Non-matching command should not be approved
	if pa.IsShellPatternApproved("go build ./...") {
		t.Error("non-matching command should not be approved")
	}

	// Verify persistence
	pa2, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if !pa2.IsShellPatternApproved("go test ./...") {
		t.Error("shell pattern should persist after reload")
	}
}

// Compound commands should be approved when every segment is covered by some
// approved pattern, even if no single pattern matches the entire command.
func TestProjectApprovals_CompoundShellPatterns(t *testing.T) {
	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, "config")

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := pa.ApproveShellPattern("gh *"); err != nil {
		t.Fatalf("ApproveShellPattern gh failed: %v", err)
	}
	if err := pa.ApproveShellPattern("echo *"); err != nil {
		t.Fatalf("ApproveShellPattern echo failed: %v", err)
	}

	cmd := `gh pr view 1 --json title && echo "---DIFF---" && gh pr diff 1`
	if !pa.IsShellPatternApproved(cmd) {
		t.Errorf("compound command covered by approved patterns should be approved: %q", cmd)
	}

	// A segment that is not covered should still cause rejection.
	unsafe := `gh pr view 1 && rm -rf /tmp/foo`
	if pa.IsShellPatternApproved(unsafe) {
		t.Errorf("compound command with uncovered segment must NOT be approved: %q", unsafe)
	}
}

func TestProjectApprovals_NilSafe(t *testing.T) {
	var pa *ProjectApprovals

	// All methods should be safe to call on nil
	if pa.IsReadApproved() {
		t.Error("nil.IsReadApproved should return false")
	}
	if pa.IsWriteApproved() {
		t.Error("nil.IsWriteApproved should return false")
	}
	if pa.IsPathApproved("/some/path", false) {
		t.Error("nil.IsPathApproved should return false")
	}
	if pa.IsShellPatternApproved("some command") {
		t.Error("nil.IsShellPatternApproved should return false")
	}

	// These should not panic
	_ = pa.ApproveRead()
	_ = pa.ApproveWrite()
	_ = pa.ApprovePath("/some/path")
	_ = pa.ApproveShellPattern("some pattern")
	_ = pa.Save()
}

func TestProjectApprovals_DuplicateApprovals(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Approve same path multiple times
	path := filepath.Join(tempDir, "src")
	if err := pa.ApprovePath(path); err != nil {
		t.Fatalf("ApprovePath failed: %v", err)
	}
	if err := pa.ApprovePath(path); err != nil {
		t.Fatalf("ApprovePath (duplicate) failed: %v", err)
	}

	// Should only have one entry
	if len(pa.ApprovedPaths) != 1 {
		t.Errorf("expected 1 approved path, got %d", len(pa.ApprovedPaths))
	}

	// Same for shell patterns
	if err := pa.ApproveShellPattern("go test *"); err != nil {
		t.Fatalf("ApproveShellPattern failed: %v", err)
	}
	if err := pa.ApproveShellPattern("go test *"); err != nil {
		t.Fatalf("ApproveShellPattern (duplicate) failed: %v", err)
	}

	if len(pa.ShellPatterns) != 1 {
		t.Errorf("expected 1 shell pattern, got %d", len(pa.ShellPatterns))
	}
}

func TestGenerateShellPattern(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{"go test ./...", "go test *"},
		{"npm install lodash", "npm install *"},
		{"git status", "git status *"},
		{"make", "make"},
		{"python script.py arg1 arg2", "python *"},
		{"ls", "ls"},
		{"ls -la", "ls *"},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := GenerateShellPattern(tt.command)
			if got != tt.want {
				t.Errorf("GenerateShellPattern(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestProjectApprovals_StorageLocation(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "test-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configDir, err := os.MkdirTemp("", "test-config-*")
	if err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	defer os.RemoveAll(configDir)

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	pa, err := LoadProjectApprovals(tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Save something
	if err := pa.ApproveRead(); err != nil {
		t.Fatalf("ApproveRead failed: %v", err)
	}

	repoSlug := slugifyApprovalPathPart(filepath.Base(tempDir))
	contextSlug := slugifyApprovalPathPart(filepath.Base(filepath.Dir(tempDir)))
	wantPath := filepath.Join(
		configDir,
		"term-llm",
		"projects",
		repoSlug,
		contextSlug+"-"+GetGitRepoID(tempDir)[:approvalRepoIDPrefixLen]+".yaml",
	)
	if pa.filePath != wantPath {
		t.Fatalf("approval file path = %q, want %q", pa.filePath, wantPath)
	}

	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected approval file at %s: %v", wantPath, err)
	}
}

func TestProjectApprovals_StorageLocationHumanReadable(t *testing.T) {
	configDir := t.TempDir()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	repoRoot := filepath.Join(string(filepath.Separator), "home", "dev", "Source", "term-llm")
	got, err := projectApprovalsFilePath(repoRoot)
	if err != nil {
		t.Fatalf("projectApprovalsFilePath failed: %v", err)
	}

	want := filepath.Join(
		configDir,
		"term-llm",
		"projects",
		"term-llm",
		"source-"+GetGitRepoID(repoRoot)[:approvalRepoIDPrefixLen]+".yaml",
	)
	if got != want {
		t.Fatalf("projectApprovalsFilePath = %q, want %q", got, want)
	}
}

func TestSlugifyApprovalPathPart(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: "", want: ""},
		{name: "all punctuation", value: "!!!", want: ""},
		{name: "trim and collapse separators", value: "  Foo__Bar...Baz  ", want: "foo-bar-baz"},
		{name: "leading trailing separators", value: "---foo---", want: "foo"},
		{name: "spaces", value: "My Project Name", want: "my-project-name"},
		{name: "unicode letters", value: "Café Déjà Vu", want: "café-déjà-vu"},
		{name: "unsupported punctuation removed", value: "foo+bar/baz", want: "foobarbaz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slugifyApprovalPathPart(tt.value); got != tt.want {
				t.Fatalf("slugifyApprovalPathPart(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestProjectApprovals_StorageLocationRootFallback(t *testing.T) {
	configDir := t.TempDir()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	repoRoot := filepath.Clean(string(filepath.Separator))
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("filepath.Abs failed: %v", err)
	}
	absRoot = filepath.Clean(absRoot)

	got, err := projectApprovalsFilePath(repoRoot)
	if err != nil {
		t.Fatalf("projectApprovalsFilePath failed: %v", err)
	}

	want := filepath.Join(
		configDir,
		"term-llm",
		"projects",
		"repo",
		"root-"+GetGitRepoID(absRoot)[:approvalRepoIDPrefixLen]+".yaml",
	)
	if got != want {
		t.Fatalf("projectApprovalsFilePath = %q, want %q", got, want)
	}
}

func TestProjectApprovals_StorageLocationIdenticalSlugsDistinctFiles(t *testing.T) {
	configDir := t.TempDir()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", configDir)
	defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

	root := t.TempDir()
	repoRoot1 := filepath.Join(root, "one", "Source", "term-llm")
	repoRoot2 := filepath.Join(root, "two", "Source", "term-llm")

	got1, err := projectApprovalsFilePath(repoRoot1)
	if err != nil {
		t.Fatalf("projectApprovalsFilePath(%q) failed: %v", repoRoot1, err)
	}
	got2, err := projectApprovalsFilePath(repoRoot2)
	if err != nil {
		t.Fatalf("projectApprovalsFilePath(%q) failed: %v", repoRoot2, err)
	}

	if got1 == got2 {
		t.Fatalf("projectApprovalsFilePath produced same path for distinct repos with identical slugs: %q", got1)
	}

	want1 := filepath.Join(
		configDir,
		"term-llm",
		"projects",
		"term-llm",
		"source-"+GetGitRepoID(repoRoot1)[:approvalRepoIDPrefixLen]+".yaml",
	)
	if got1 != want1 {
		t.Fatalf("projectApprovalsFilePath(%q) = %q, want %q", repoRoot1, got1, want1)
	}

	want2 := filepath.Join(
		configDir,
		"term-llm",
		"projects",
		"term-llm",
		"source-"+GetGitRepoID(repoRoot2)[:approvalRepoIDPrefixLen]+".yaml",
	)
	if got2 != want2 {
		t.Fatalf("projectApprovalsFilePath(%q) = %q, want %q", repoRoot2, got2, want2)
	}
}
