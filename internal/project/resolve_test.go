package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveGitSubdirectoryUsesMainRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "nested", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", repo)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	resolved, err := Resolve(context.Background(), filepath.Join(repo, "nested", "dir"))
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Git || !SamePath(resolved.CanonicalDir, repo) {
		t.Fatalf("resolved = %#v, want Git root %q", resolved, repo)
	}
	if !MatchesWorkspace(context.Background(), filepath.Join(repo, "nested"), "", resolved) {
		t.Fatal("Git subdirectory did not match its project")
	}
}

func TestMatchesWorkspaceRequiresExactNonGitDirectory(t *testing.T) {
	root := t.TempDir()
	resolved, err := Resolve(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Git {
		t.Skip("temporary directory unexpectedly belongs to a Git repository")
	}
	if !MatchesWorkspace(context.Background(), root, "", resolved) {
		t.Fatal("exact non-Git directory did not match")
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if MatchesWorkspace(context.Background(), nested, "", resolved) {
		t.Fatal("nested non-Git directory unexpectedly matched")
	}
}
