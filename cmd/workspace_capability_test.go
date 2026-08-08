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
	server := &serveServer{store: store}
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
