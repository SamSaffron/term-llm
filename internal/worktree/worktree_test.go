package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/appdata"
)

// initRepo creates a fresh git repo with one commit and returns its root.
func initRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runOrFail(t, root, "git", "init", "-q")
	runOrFail(t, root, "git", "config", "user.email", "test@example.com")
	runOrFail(t, root, "git", "config", "user.name", "Test")
	runOrFail(t, root, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runOrFail(t, root, "git", "add", "README.md")
	runOrFail(t, root, "git", "commit", "-qm", "init")
	return root
}

func runOrFail(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// guardXDG points the data dir at a temp location and verifies the package
// honors it, so tests never touch the real data directory.
func guardXDG(t *testing.T) {
	t.Helper()
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("XDG_DATA_HOME", xdg)
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		t.Fatalf("data dir: %v", err)
	}
	if !strings.HasPrefix(dataDir, xdg) {
		t.Skipf("data dir %q does not honor XDG_DATA_HOME %q; skipping to avoid polluting real data dir", dataDir, xdg)
	}
}

func TestCreateListGetRemove(t *testing.T) {
	guardXDG(t)
	repo := initRepo(t)
	ctx := context.Background()

	wt, err := Create(ctx, repo, CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !wt.Detached {
		t.Errorf("expected detached HEAD, got branch %q", wt.Branch)
	}
	if wt.Status != StatusReady {
		t.Errorf("status = %q, want ready", wt.Status)
	}
	if wt.DirtyFiles != 0 {
		t.Errorf("DirtyFiles = %d, want 0", wt.DirtyFiles)
	}
	if _, err := os.Stat(wt.Dir); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}
	if !strings.Contains(wt.Dir, "worktrees") {
		t.Errorf("dir %q not under managed root", wt.Dir)
	}

	list, err := List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	if !pathsEqual(list[0].Dir, wt.Dir) {
		t.Errorf("List dir = %q, want %q", list[0].Dir, wt.Dir)
	}
	if list[0].Name != wt.Name {
		t.Errorf("List name = %q, want %q", list[0].Name, wt.Name)
	}

	got, err := Get(wt.Dir)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != wt.Name {
		t.Errorf("Get name = %q, want %q", got.Name, wt.Name)
	}
	if got.Base != wt.Base || got.Base == "" {
		t.Errorf("Get base = %q, want %q", got.Base, wt.Base)
	}
	if !got.Detached {
		t.Errorf("Get should report detached")
	}

	if err := Remove(wt.Dir, false); err != nil {
		t.Fatalf("Remove(clean): %v", err)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Errorf("worktree dir still present after Remove")
	}
	list, _ = List(repo)
	if len(list) != 0 {
		t.Errorf("List len = %d after remove, want 0", len(list))
	}
}

func TestSetupScriptRunsAndDirtyGuard(t *testing.T) {
	guardXDG(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, CreateOptions{
		SetupScript: "echo marker > setup_marker.txt",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "setup_marker.txt")); err != nil {
		t.Fatalf("setup script did not run: %v", err)
	}

	// The untracked marker makes the worktree dirty; a non-forced Remove must refuse.
	got, _ := Get(wt.Dir)
	if got.DirtyFiles == 0 {
		t.Errorf("expected dirty worktree after setup script")
	}
	if err := Remove(wt.Dir, false); err == nil {
		t.Errorf("Remove(dirty, force=false) should fail")
	}
	if err := Remove(wt.Dir, true); err != nil {
		t.Fatalf("Remove(dirty, force=true): %v", err)
	}
}

func TestSetupScriptFailureCleansUp(t *testing.T) {
	guardXDG(t)
	repo := initRepo(t)

	_, err := Create(context.Background(), repo, CreateOptions{
		Name:        "doomed",
		SetupScript: "exit 3",
	})
	if err == nil {
		t.Fatalf("Create should fail when setup script fails")
	}
	root, _ := ManagedRoot(repo)
	if _, statErr := os.Stat(filepath.Join(root, "doomed")); !os.IsNotExist(statErr) {
		t.Errorf("half-created worktree dir left behind")
	}
	if list, _ := List(repo); len(list) != 0 {
		t.Errorf("List len = %d after failed create, want 0", len(list))
	}
}

func TestPromoteAndForceRemoveDeletesBranch(t *testing.T) {
	guardXDG(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, CreateOptions{Name: "promoteme"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := Promote(wt.Dir, "feature/x"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	got, err := Get(wt.Dir)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Detached {
		t.Errorf("worktree should be attached after promote")
	}
	if got.Branch != "feature/x" {
		t.Errorf("Branch = %q, want feature/x", got.Branch)
	}

	if err := Remove(wt.Dir, true); err != nil {
		t.Fatalf("Remove(force): %v", err)
	}
	out := runOrFail(t, repo, "git", "branch", "--list", "feature/x")
	if strings.TrimSpace(out) != "" {
		t.Errorf("branch feature/x not deleted: %q", out)
	}
}

func TestDiff(t *testing.T) {
	guardXDG(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(wt.Dir, true) })

	if err := os.WriteFile(filepath.Join(wt.Dir, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("modify file: %v", err)
	}
	diff, err := Diff(wt.Dir)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "+world") {
		t.Errorf("diff missing change:\n%s", diff)
	}
}

func TestDuplicateNameErrors(t *testing.T) {
	guardXDG(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, CreateOptions{Name: "fixed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(wt.Dir, true) })

	_, err = Create(context.Background(), repo, CreateOptions{Name: "fixed"})
	if err == nil {
		t.Fatalf("duplicate name should error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want already-exists", err)
	}
}

func TestNonRepoIsInert(t *testing.T) {
	dir := t.TempDir()
	if IsGitRepo(dir) {
		t.Errorf("temp dir should not be a git repo")
	}
	if _, err := List(dir); err == nil {
		t.Errorf("List on non-repo should error")
	}
}

func TestParseWorktreeList(t *testing.T) {
	sample := strings.Join([]string{
		"worktree /home/u/repo",
		"HEAD 1111111111111111111111111111111111111111",
		"branch refs/heads/main",
		"",
		"worktree /data/worktrees/abcd/neon-canyon",
		"HEAD 2222222222222222222222222222222222222222",
		"detached",
		"",
	}, "\n")
	recs := parseWorktreeList(sample)
	if len(recs) != 2 {
		t.Fatalf("parsed %d records, want 2", len(recs))
	}
	if recs[0].Branch != "main" || recs[0].Detached {
		t.Errorf("rec0 = %+v, want branch main attached", recs[0])
	}
	if recs[1].Dir != "/data/worktrees/abcd/neon-canyon" || !recs[1].Detached {
		t.Errorf("rec1 = %+v, want detached managed path", recs[1])
	}
	if recs[1].Branch != "" {
		t.Errorf("rec1 branch = %q, want empty (detached)", recs[1].Branch)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"  Hello World ": "hello-world",
		"feature/Foo":    "feature-foo",
		"a__b":           "a-b",
		"!!!":            "",
		"Neon-Canyon":    "neon-canyon",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugShape(t *testing.T) {
	s := Slug()
	parts := strings.Split(s, "-")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Errorf("Slug() = %q, want adjective-noun", s)
	}
}
