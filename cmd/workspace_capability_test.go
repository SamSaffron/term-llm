package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestServeSessionDoesNotTrustPersistedDaemonCWD(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	staleDaemonCWD := t.TempDir()
	trustedRoot := newGitRepoForBindingTest(t)
	now := time.Now()
	sess := &session.Session{ID: "web-stale-cwd", Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: session.OriginWeb, CWD: staleDaemonCWD, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	toolMgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{tools.ReadFileToolName}, BaseDir: staleDaemonCWD, PrimaryWorkspace: staleDaemonCWD}, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{toolMgr: toolMgr}
	server := &serveServer{
		cfg:            serveServerConfig{ui: true},
		store:          store,
		worktreeRootFn: func() (string, error) { return trustedRoot, nil },
	}
	if err := server.ensureRuntimeBaseDirForSession(ctx, sess.ID, rt); err != nil {
		t.Fatal(err)
	}
	if capabilities := toolMgr.ApprovalMgr.WorkspaceCapabilities(); len(capabilities) != 0 {
		t.Fatalf("serve trusted persisted daemon cwd: %#v", capabilities)
	}
	if toolMgr.BaseDir() != "" {
		t.Fatalf("serve retained stale BaseDir %q", toolMgr.BaseDir())
	}
}

func TestServeSessionDoesNotImplicitlyBindOutsideWebUIOrigin(t *testing.T) {
	ctx := context.Background()
	root := newGitRepoForBindingTest(t)
	for _, tc := range []struct {
		name   string
		ui     bool
		origin session.SessionOrigin
	}{
		{name: "api only", ui: false, origin: session.OriginWeb},
		{name: "unbound web session", ui: true, origin: session.OriginWeb},
		{name: "non web session", ui: true, origin: session.OriginTUI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Now()
			sess := &session.Session{ID: "unbound", Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: tc.origin, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
			if err := store.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			toolMgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{tools.ReadFileToolName}, BaseDir: root, PrimaryWorkspace: root, RequireExplicitWorkingDir: true}, &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			server := &serveServer{cfg: serveServerConfig{ui: tc.ui}, store: store, worktreeRootFn: func() (string, error) { return root, nil }}
			if err := server.ensureRuntimeBaseDirForSession(ctx, sess.ID, &serveRuntime{toolMgr: toolMgr}); err != nil {
				t.Fatal(err)
			}
			if toolMgr.BaseDir() != "" {
				t.Fatalf("implicitly bound BaseDir %q", toolMgr.BaseDir())
			}
		})
	}
}

func TestServeSessionRestoresPersistedValidatedWebRoot(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := newGitRepoForBindingTest(t)
	now := time.Now()
	sess := &session.Session{ID: "web-root", Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: session.OriginWeb, CWD: root, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	toolMgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{tools.ReadFileToolName}, RequireExplicitWorkingDir: true}, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{toolMgr: toolMgr}
	server := &serveServer{
		cfg:            serveServerConfig{ui: true},
		store:          store,
		worktreeRootFn: func() (string, error) { return root, nil },
	}
	if err := server.ensureRuntimeBaseDirForSession(ctx, sess.ID, rt); err != nil {
		t.Fatal(err)
	}
	if !sameServePath(toolMgr.BaseDir(), root) {
		t.Fatalf("serve root BaseDir = %q, want %q", toolMgr.BaseDir(), root)
	}
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || !sameServePath(persisted.CWD, root) || persisted.WorktreeDir != "" {
		t.Fatalf("persisted root binding = %#v, want CWD %q and no worktree", persisted, root)
	}
}
