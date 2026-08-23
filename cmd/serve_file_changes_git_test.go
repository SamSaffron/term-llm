package cmd

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/gitdiff"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func TestSessionFileChangesGitScopes(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepoForBindingTest(t)
	path := filepath.Join(repo, "scope-test.txt")
	if err := os.WriteFile(path, []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForBindingTest(t, repo, "add", "scope-test.txt")
	if err := os.WriteFile(path, []byte("working\nextra\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessions.Close() })
	if err := sessions.Create(ctx, &session.Session{ID: "git-session", CWD: repo, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	files, err := filetrack.Open(filepath.Join(t.TempDir(), "files.db"), filetrack.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	srv := &serveServer{store: sessions, fileTrackStoreFn: func() *filetrack.Store { return files }}

	for _, tc := range []struct {
		scope string
		adds  float64
		dels  float64
	}{
		{scope: fileChangeScopeStaged, adds: 1, dels: 0},
		{scope: fileChangeScopeUnstaged, adds: 2, dels: 1},
		{scope: fileChangeScopeUncommitted, adds: 2, dels: 0},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			code, body := getSessionPath(t, srv, "/v1/sessions/git-session/file-changes?scope="+tc.scope)
			if code != http.StatusOK || body["git"] != true || body["scope"] != tc.scope {
				t.Fatalf("list status=%d body=%#v", code, body)
			}
			changes := body["file_changes"].([]any)
			if len(changes) != 1 {
				t.Fatalf("changes = %#v", changes)
			}
			change := changes[0].(map[string]any)
			if change["path"] != path || change["adds"] != tc.adds || change["dels"] != tc.dels {
				t.Fatalf("change = %#v, want path %q +%v -%v", change, path, tc.adds, tc.dels)
			}

			code, diff := getSessionPath(t, srv, "/v1/sessions/git-session/file-changes/diff?scope="+tc.scope+"&path="+path)
			if code != http.StatusOK || diff["kind"] == nil {
				t.Fatalf("diff status=%d body=%#v", code, diff)
			}
		})
	}
}

func TestSessionGitRepoUsesBoundWorktreeCheckout(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	srv, store := newServeProjectTestServer(t)
	mainRepo := newGitRepoForBindingTest(t)
	project := &session.Project{Name: "Worktree project", CanonicalDir: mainRepo}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	managed, err := worktree.Create(ctx, mainRepo, worktree.CreateOptions{Name: "diff-scope"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background(), managed.Dir, worktree.RemoveOptions{Force: true}) })

	mainOnly := filepath.Join(mainRepo, "main-only.txt")
	worktreeOnly := filepath.Join(managed.Dir, "worktree-only.txt")
	if err := os.WriteFile(mainOnly, []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worktreeOnly, []byte("worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sess := &session.Session{
		ID: "worktree-session", ProjectID: project.ID, CWD: managed.Dir, WorktreeDir: managed.Dir,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	repo, ok := srv.sessionGitRepo(ctx, sess.ID)
	if !ok {
		t.Fatal("bound worktree was not recognized as a Git checkout")
	}
	changes, err := repo.List(ctx, gitdiff.ScopeUncommitted)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != worktreeOnly {
		t.Fatalf("worktree changes = %#v, want only %q", changes, worktreeOnly)
	}
}

func TestSessionFileChangesRejectGitScopeOutsideRepository(t *testing.T) {
	srv, _ := newFileChangesTestServer(t)
	srv.startupDir = t.TempDir()
	code, _ := getSessionPath(t, srv, "/v1/sessions/sess-1/file-changes?scope=staged")
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", code)
	}
}
