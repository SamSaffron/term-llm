package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func newGitRepoForBindingTest(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll repo: %v", err)
	}
	runGitForBindingTest(t, repo, "init")
	runGitForBindingTest(t, repo, "config", "user.email", "test@example.com")
	runGitForBindingTest(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitForBindingTest(t, repo, "add", "file.txt")
	runGitForBindingTest(t, repo, "commit", "-m", "init")
	return repo
}

func runGitForBindingTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("git %v failed: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out)
}

func TestBindWorktreeSessionUsesToolManagerBaseDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	repo := newGitRepoForBindingTest(t)
	worktreeDir := filepath.Join(t.TempDir(), "linked")
	runGitForBindingTest(t, repo, "worktree", "add", "--detach", worktreeDir, "HEAD")
	t.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "remove", "--force", worktreeDir)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		_ = cmd.Run()
	})

	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: "sess-bind", Provider: "mock", Model: "tiny", Mode: session.ModeChat, CWD: repo, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	toolCfg := tools.DefaultToolConfig()
	toolCfg.Enabled = []string{tools.ReadFileToolName}
	mgr, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}

	if err := BindWorktreeSession(ctx, store, sess, mgr, worktreeDir); err != nil {
		t.Fatalf("BindWorktreeSession: %v", err)
	}
	if mgr.BaseDir() != worktreeDir {
		t.Fatalf("BaseDir = %q, want %q", mgr.BaseDir(), worktreeDir)
	}
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if persisted.WorktreeDir != worktreeDir || persisted.CWD != worktreeDir {
		t.Fatalf("persisted worktree/cwd = %q/%q, want %q", persisted.WorktreeDir, persisted.CWD, worktreeDir)
	}

	if err := BindRootSession(ctx, store, persisted, mgr, repo); err != nil {
		t.Fatalf("BindRootSession: %v", err)
	}
	if mgr.BaseDir() != repo {
		t.Fatalf("root BaseDir = %q, want %q", mgr.BaseDir(), repo)
	}
	persisted, err = store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after root bind: %v", err)
	}
	if persisted.WorktreeDir != "" || persisted.CWD != repo {
		t.Fatalf("root persisted worktree/cwd = %q/%q, want empty/%q", persisted.WorktreeDir, persisted.CWD, repo)
	}
}

func TestSyncPersistedSessionRuntimeBindsWorktreeDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	repo := newGitRepoForBindingTest(t)
	worktreeDir := filepath.Join(t.TempDir(), "linked-sync")
	runGitForBindingTest(t, repo, "worktree", "add", "--detach", worktreeDir, "HEAD")
	t.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "remove", "--force", worktreeDir)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		_ = cmd.Run()
	})

	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	toolCfg := tools.DefaultToolConfig()
	toolCfg.Enabled = []string{tools.ReadFileToolName}
	mgr, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}

	srv := &serveServer{store: store}
	rt := &serveRuntime{toolMgr: mgr, defaultModel: "tiny", providerKey: "mock"}
	srv.syncPersistedSessionRuntime(ctx, "sess-sync-wt", rt, "tiny", "", worktreeDir)

	persisted, err := store.Get(ctx, "sess-sync-wt")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if persisted == nil {
		t.Fatal("session was not created")
	}
	if persisted.WorktreeDir != worktreeDir || persisted.CWD != worktreeDir {
		t.Fatalf("persisted worktree/cwd = %q/%q, want %q", persisted.WorktreeDir, persisted.CWD, worktreeDir)
	}
	if mgr.BaseDir() != worktreeDir {
		t.Fatalf("tool manager BaseDir = %q, want %q", mgr.BaseDir(), worktreeDir)
	}
}

func TestSyncPersistedSessionRuntimeDoesNotRetargetConflictingWorktree(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	repo := newGitRepoForBindingTest(t)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	runGitForBindingTest(t, repo, "worktree", "add", "--detach", first, "HEAD")
	runGitForBindingTest(t, repo, "worktree", "add", "--detach", second, "HEAD")
	t.Cleanup(func() {
		for _, dir := range []string{first, second} {
			cmd := exec.Command("git", "worktree", "remove", "--force", dir)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			_ = cmd.Run()
		}
	})

	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	if err := store.Create(ctx, &session.Session{
		ID:          "sess-conflict",
		Provider:    "mock",
		ProviderKey: "mock",
		Model:       "tiny",
		Mode:        session.ModeChat,
		Origin:      session.OriginWeb,
		CWD:         first,
		WorktreeDir: first,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Status:      session.StatusActive,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	toolCfg := tools.DefaultToolConfig()
	toolCfg.Enabled = []string{tools.ReadFileToolName}
	mgr, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}
	if err := mgr.SetBaseDir(first); err != nil {
		t.Fatalf("SetBaseDir first: %v", err)
	}

	srv := &serveServer{store: store}
	rt := &serveRuntime{toolMgr: mgr, defaultModel: "tiny", providerKey: "mock"}
	srv.syncPersistedSessionRuntime(ctx, "sess-conflict", rt, "tiny", "", second)

	persisted, err := store.Get(ctx, "sess-conflict")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if persisted.WorktreeDir != first || persisted.CWD != first {
		t.Fatalf("persisted worktree/cwd = %q/%q, want %q", persisted.WorktreeDir, persisted.CWD, first)
	}
	if mgr.BaseDir() != first {
		t.Fatalf("tool manager BaseDir = %q, want %q", mgr.BaseDir(), first)
	}
}

func TestRestoreWorktreeBindingFallsBackFromStaleCWD(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	repo := newGitRepoForBindingTest(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir repo: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	sess := &session.Session{
		ID:        "stale-cwd",
		Provider:  "mock",
		Model:     "tiny",
		Mode:      session.ModeChat,
		CWD:       filepath.Join(t.TempDir(), "gone"),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	toolCfg := tools.DefaultToolConfig()
	toolCfg.Enabled = []string{tools.ReadFileToolName}
	mgr, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}

	if err := RestoreWorktreeBinding(ctx, store, sess, mgr); err != nil {
		t.Fatalf("RestoreWorktreeBinding: %v", err)
	}
	if !sameServePath(mgr.BaseDir(), repo) {
		t.Fatalf("BaseDir = %q, want repo %q", mgr.BaseDir(), repo)
	}
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if persisted.WorktreeDir != "" || !sameServePath(persisted.CWD, repo) {
		t.Fatalf("persisted worktree/cwd = %q/%q, want empty/%q", persisted.WorktreeDir, persisted.CWD, repo)
	}
}
