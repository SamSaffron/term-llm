package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitIdentityRequiresRegisteredWorktree(t *testing.T) {
	trusted := t.TempDir()
	runGitTestCommand(t, trusted, "init", "-q")
	runGitTestCommand(t, trusted, "commit", "--allow-empty", "-q", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	runGitTestCommand(t, trusted, "worktree", "add", "--detach", linked, "HEAD")
	trustedID := GetGitRepoID(trusted)

	for _, name := range []string{"unregistered_admin", "symlink_main_git", "symlink_worktree_git", "trailing_space_root"} {
		t.Run(name, func(t *testing.T) {
			hostile := t.TempDir()
			switch name {
			case "unregistered_admin":
				admin := filepath.Join(hostile, "admin")
				if err := os.Mkdir(admin, 0o755); err != nil {
					t.Fatal(err)
				}
				// Even a self-consistent gitdir/backlink pair cannot register
				// itself in the trusted repository by claiming its commondir.
				for path, contents := range map[string]string{
					filepath.Join(hostile, ".git"):    "gitdir: " + admin + "\n",
					filepath.Join(admin, "commondir"): filepath.Join(trusted, ".git") + "\n",
					filepath.Join(admin, "gitdir"):    filepath.Join(hostile, ".git") + "\n",
					filepath.Join(admin, "HEAD"):      "ref: refs/heads/master\n",
				} {
					if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink_main_git", "symlink_worktree_git":
				target := filepath.Join(trusted, ".git")
				if name == "symlink_worktree_git" {
					target = filepath.Join(linked, ".git")
				}
				if err := os.Symlink(target, filepath.Join(hostile, ".git")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "trailing_space_root":
				// Trimming Git's output as whitespace would alias this path
				// to the trusted root before registration validation.
				hostile = trusted + " "
				if err := os.Mkdir(hostile, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(hostile) })
				if err := os.WriteFile(filepath.Join(hostile, ".git"), []byte("gitdir: "+filepath.Join(trusted, ".git")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			runGitTestCommand(t, hostile, "rev-parse", "--git-common-dir")
			if got := GetGitRepoID(hostile); got == trustedID {
				t.Fatal("unregistered checkout inherited trusted identity")
			}
		})
	}
}

func TestGitIdentityPreservesRelativeWorktreeRegistration(t *testing.T) {
	trusted := t.TempDir()
	runGitTestCommand(t, trusted, "init", "-q")
	runGitTestCommand(t, trusted, "commit", "--allow-empty", "-q", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	runGitTestCommand(t, trusted, "worktree", "add", "--detach", linked, "HEAD")
	admin := filepath.Join(trusted, ".git", "worktrees", "linked")
	toAdmin, err := filepath.Rel(linked, admin)
	if err != nil {
		t.Fatal(err)
	}
	toGitFile, err := filepath.Rel(admin, filepath.Join(linked, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(linked, ".git"):  "gitdir: " + toAdmin + "\n",
		filepath.Join(admin, "gitdir"): toGitFile + "\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := GetGitRepoID(linked), GetGitRepoID(trusted); got != want {
		t.Fatalf("relative registered worktree identity = %s, want %s", got, want)
	}
}
