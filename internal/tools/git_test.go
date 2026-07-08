package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func setGitCeiling(t *testing.T, ceiling string) {
	t.Helper()

	if existing := os.Getenv("GIT_CEILING_DIRECTORIES"); existing != "" {
		ceiling += string(os.PathListSeparator) + existing
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", ceiling)
}

func TestDetectGitRepo(t *testing.T) {
	// Test with the actual term-llm repo (we know we're in one)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}

	info := DetectGitRepo(cwd)

	// We should be in a git repo
	if !info.IsRepo {
		t.Skip("not running in a git repo, skipping test")
	}

	if info.Root == "" {
		t.Error("expected non-empty Root")
	}

	if info.RepoName == "" {
		t.Error("expected non-empty RepoName")
	}

	// Root should be an absolute path
	if !filepath.IsAbs(info.Root) {
		t.Errorf("expected absolute path, got %s", info.Root)
	}
}

func TestDetectGitRepo_NotInRepo(t *testing.T) {
	// Create a temp directory that's not a git repo
	tempDir, err := os.MkdirTemp("", "not-a-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var) so the git ceiling matches
	// the path git sees when walking parent directories.
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}
	setGitCeiling(t, filepath.Dir(tempDir))

	info := DetectGitRepo(tempDir)

	if info.IsRepo {
		t.Error("expected IsRepo to be false for non-repo directory")
	}

	if info.Root != "" {
		t.Errorf("expected empty Root, got %s", info.Root)
	}
}

func TestDetectGitRepo_NewRepo(t *testing.T) {
	// Create a new git repo in temp directory
	tempDir, err := os.MkdirTemp("", "new-repo-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed, skipping: %v", err)
	}

	info := DetectGitRepo(tempDir)

	if !info.IsRepo {
		t.Error("expected IsRepo to be true for new git repo")
	}

	if info.Root != tempDir {
		t.Errorf("expected Root=%s, got %s", tempDir, info.Root)
	}

	if info.RepoName != filepath.Base(tempDir) {
		t.Errorf("expected RepoName=%s, got %s", filepath.Base(tempDir), info.RepoName)
	}
}

func TestDetectGitRepo_Subdirectory(t *testing.T) {
	// Create a new git repo with subdirectory
	tempDir, err := os.MkdirTemp("", "repo-subdir-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed, skipping: %v", err)
	}

	// Create subdirectory
	subDir := filepath.Join(tempDir, "src", "internal")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	info := DetectGitRepo(subDir)

	if !info.IsRepo {
		t.Error("expected IsRepo to be true for subdirectory of git repo")
	}

	if info.Root != tempDir {
		t.Errorf("expected Root=%s, got %s", tempDir, info.Root)
	}
}

func TestDetectGitRepo_NonexistentPathInRepo(t *testing.T) {
	// Create a new git repo in temp directory
	tempDir, err := os.MkdirTemp("", "repo-missing-path-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var)
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed, skipping: %v", err)
	}

	target := filepath.Join(tempDir, "new", "deep", "dir", "file.go")
	info := DetectGitRepo(target)

	if !info.IsRepo {
		t.Error("expected IsRepo to be true for nonexistent path inside git repo")
	}

	if info.Root != tempDir {
		t.Errorf("expected Root=%s, got %s", tempDir, info.Root)
	}
}

func TestDetectGitRepo_NonexistentPathNotInRepo(t *testing.T) {
	// Create a temp directory that's not a git repo
	tempDir, err := os.MkdirTemp("", "not-a-repo-missing-path-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Resolve symlinks (macOS /var -> /private/var) so the git ceiling matches
	// the path git sees when walking parent directories.
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}
	setGitCeiling(t, filepath.Dir(tempDir))

	target := filepath.Join(tempDir, "missing", "deep", "file.go")
	info := DetectGitRepo(target)

	if info.IsRepo {
		t.Error("expected IsRepo to be false for nonexistent path outside git repo")
	}

	if info.Root != "" {
		t.Errorf("expected empty Root, got %s", info.Root)
	}
}

func TestGetGitRepoID(t *testing.T) {
	// Same root should produce same ID
	id1 := GetGitRepoID("/some/path")
	id2 := GetGitRepoID("/some/path")

	if id1 != id2 {
		t.Error("same path should produce same ID")
	}

	// Different roots should produce different IDs
	id3 := GetGitRepoID("/other/path")

	if id1 == id3 {
		t.Error("different paths should produce different IDs")
	}

	// ID should be hex string of reasonable length
	if len(id1) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("expected ID length 32, got %d", len(id1))
	}
}

func TestGetGitRepoIDCanonicalizesLinkedWorktree(t *testing.T) {
	repo := t.TempDir()
	runGitTestCommand(t, repo, "init")
	runGitTestCommand(t, repo, "config", "user.email", "test@example.com")
	runGitTestCommand(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitTestCommand(t, repo, "add", "README.md")
	runGitTestCommand(t, repo, "commit", "-m", "init")

	worktreeDir := filepath.Join(t.TempDir(), "linked")
	runGitTestCommand(t, repo, "worktree", "add", "--detach", worktreeDir, "HEAD")
	t.Cleanup(func() { _, _ = runGitTestCommandAllowError(repo, "worktree", "remove", "--force", worktreeDir) })

	rootID := GetGitRepoID(repo)
	worktreeID := GetGitRepoID(worktreeDir)
	if rootID != worktreeID {
		t.Fatalf("worktree repo ID = %s, want root ID %s", worktreeID, rootID)
	}
}

func runGitTestCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := runGitTestCommandAllowError(dir, args...)
	if err != nil {
		t.Skipf("git %v failed: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
}

func runGitTestCommandAllowError(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	return cmd.CombinedOutput()
}

func TestIsPathInRepo(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		repoRoot string
		want     bool
	}{
		{
			name:     "exact match",
			path:     "/repo",
			repoRoot: "/repo",
			want:     true,
		},
		{
			name:     "file in repo",
			path:     "/repo/src/main.go",
			repoRoot: "/repo",
			want:     true,
		},
		{
			name:     "nested directory",
			path:     "/repo/src/internal/tools",
			repoRoot: "/repo",
			want:     true,
		},
		{
			name:     "outside repo",
			path:     "/other/file.go",
			repoRoot: "/repo",
			want:     false,
		},
		{
			name:     "parent of repo",
			path:     "/",
			repoRoot: "/repo",
			want:     false,
		},
		{
			name:     "similar prefix but different",
			path:     "/repo-other/file.go",
			repoRoot: "/repo",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPathInRepo(tt.path, tt.repoRoot)
			if got != tt.want {
				t.Errorf("IsPathInRepo(%q, %q) = %v, want %v", tt.path, tt.repoRoot, got, tt.want)
			}
		})
	}
}

func TestGetRelativePath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		repoRoot string
		want     string
	}{
		{
			name:     "simple file",
			path:     "/repo/main.go",
			repoRoot: "/repo",
			want:     "main.go",
		},
		{
			name:     "nested file",
			path:     "/repo/src/internal/tools/file.go",
			repoRoot: "/repo",
			want:     "src/internal/tools/file.go",
		},
		{
			name:     "same as root",
			path:     "/repo",
			repoRoot: "/repo",
			want:     ".",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetRelativePath(tt.path, tt.repoRoot)
			if got != tt.want {
				t.Errorf("GetRelativePath(%q, %q) = %q, want %q", tt.path, tt.repoRoot, got, tt.want)
			}
		})
	}
}
