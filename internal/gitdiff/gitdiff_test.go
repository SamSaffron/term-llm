package gitdiff

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepoScopes(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	writeGitFile(t, root, "tracked.txt", "base\n")
	writeGitFile(t, root, "delete.txt", "delete me\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "base")

	writeGitFile(t, root, "tracked.txt", "staged\n")
	runGit(t, root, "add", "tracked.txt")
	writeGitFile(t, root, "tracked.txt", "working\nextra\n")
	writeGitFile(t, root, "new.txt", "new\n")
	if err := os.Remove(filepath.Join(root, "delete.txt")); err != nil {
		t.Fatal(err)
	}

	repo, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		scope Scope
		want  map[string][3]any
	}{
		{ScopeStaged, map[string][3]any{"tracked.txt": {"modify", 1, 1}}},
		{ScopeUnstaged, map[string][3]any{
			"delete.txt":  {"delete", 0, 1},
			"new.txt":     {"create", 1, 0},
			"tracked.txt": {"modify", 2, 1},
		}},
		{ScopeUncommitted, map[string][3]any{
			"delete.txt":  {"delete", 0, 1},
			"new.txt":     {"create", 1, 0},
			"tracked.txt": {"modify", 2, 1},
		}},
	}
	for _, tc := range tests {
		t.Run(string(tc.scope), func(t *testing.T) {
			changes, err := repo.List(context.Background(), tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != len(tc.want) {
				t.Fatalf("changes = %#v, want %d", changes, len(tc.want))
			}
			for _, change := range changes {
				rel, err := filepath.Rel(root, change.Path)
				if err != nil {
					t.Fatal(err)
				}
				want, ok := tc.want[filepath.ToSlash(rel)]
				if !ok {
					t.Fatalf("unexpected path %q", rel)
				}
				if change.Kind != want[0] || change.Adds != want[1] || change.Dels != want[2] || !change.ContentAvailable {
					t.Fatalf("%s = kind %s +%d -%d available=%t, want %#v and available", rel, change.Kind, change.Adds, change.Dels, change.ContentAvailable, want)
				}
			}
		})
	}

	content, err := repo.File(context.Background(), ScopeStaged, filepath.Join(root, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if content == nil || !content.ContentAvailable || string(content.Before) != "base\n" || string(content.After) != "staged\n" {
		t.Fatalf("staged content = %#v", content)
	}
	if content, err := repo.File(context.Background(), ScopeStaged, filepath.Join(root, "new.txt")); err != nil || content != nil {
		t.Fatalf("unstaged path in staged scope = %#v, %v", content, err)
	}
	untracked, err := repo.File(context.Background(), ScopeUnstaged, filepath.Join(root, "new.txt"))
	if err != nil || untracked == nil || !untracked.ContentAvailable || untracked.Kind != "create" || string(untracked.After) != "new\n" {
		t.Fatalf("untracked content = %#v, %v", untracked, err)
	}
}

func TestRepoSupportsUnbornRepository(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	writeGitFile(t, root, "new.txt", "new\n")
	repo, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.List(context.Background(), ScopeUncommitted)
	if err != nil || len(changes) != 1 || changes[0].Kind != "create" {
		t.Fatalf("unborn changes = %#v, %v", changes, err)
	}
	content, err := repo.File(context.Background(), ScopeUncommitted, "new.txt")
	if err != nil || content == nil || content.Kind != "create" {
		t.Fatalf("unborn content = %#v, %v", content, err)
	}

	runGit(t, root, "add", "new.txt")
	changes, err = repo.List(context.Background(), ScopeStaged)
	if err != nil || len(changes) != 1 || changes[0].Kind != "create" {
		t.Fatalf("unborn staged changes = %#v, %v", changes, err)
	}
	content, err = repo.File(context.Background(), ScopeStaged, "new.txt")
	if err != nil || content == nil || content.Kind != "create" || string(content.After) != "new\n" {
		t.Fatalf("unborn staged content = %#v, %v", content, err)
	}
}

func TestRepoRejectsOutsidePath(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	repo, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.File(context.Background(), ScopeUnstaged, filepath.Join(root, "..", "outside")); err == nil {
		t.Fatal("outside path accepted")
	}
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeGitFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
