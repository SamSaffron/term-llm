package chat

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestPersistNewTUISessionAssignsRegisteredProject(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	root := t.TempDir()
	p := &session.Project{Name: "TUI project", CanonicalDir: root}
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	// Keep this focused on assignment rather than the asynchronous history repair.
	reconciledTUIProjects.Store(p.ID, struct{}{})
	defer reconciledTUIProjects.Delete(p.ID)

	now := time.Now()
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeChat, Origin: session.OriginTUI, CWD: root, CreatedAt: now, UpdatedAt: now}
	persistNewTUISession(ctx, store, sess)
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ProjectID != p.ID || persisted.ProjectName != p.Name {
		t.Fatalf("persisted project = %q %q, want %q %q", persisted.ProjectID, persisted.ProjectName, p.ID, p.Name)
	}
}

func TestPersistNewTUISessionLeavesUnregisteredDirectoryUnassigned(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", CWD: t.TempDir(), CreatedAt: now, UpdatedAt: now}
	persistNewTUISession(context.Background(), store, sess)
	persisted, err := store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ProjectID != "" {
		t.Fatalf("unexpected project assignment: %#v", persisted)
	}
}
