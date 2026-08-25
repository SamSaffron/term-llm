package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func newAssignmentStore(t *testing.T) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAssignSessionForDirAndReconcileHistory(t *testing.T) {
	store := newAssignmentStore(t)
	ctx := context.Background()
	root := t.TempDir()
	p := &session.Project{Name: "Automatic", CanonicalDir: root}
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	legacy := &session.Session{ID: "legacy-auto-project", Provider: "mock", Model: "mock", CWD: root, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	fresh := &session.Session{ID: "fresh-auto-project", CWD: root}
	matched, err := AssignSessionForDir(ctx, store, fresh, root)
	if err != nil || matched == nil || fresh.ProjectID != p.ID || fresh.ProjectName != p.Name {
		t.Fatalf("fresh assignment = project %#v session %#v, %v", matched, fresh, err)
	}

	claimed, err := ReconcileAll(ctx, store)
	if err != nil || claimed != 1 {
		t.Fatalf("first reconciliation = %d, %v; want 1", claimed, err)
	}
	claimed, err = ReconcileAll(ctx, store)
	if err != nil || claimed != 0 {
		t.Fatalf("second reconciliation = %d, %v; want 0", claimed, err)
	}
	persisted, err := store.Get(ctx, legacy.ID)
	if err != nil || persisted.ProjectID != p.ID {
		t.Fatalf("legacy session = %#v, %v", persisted, err)
	}
}

func TestReconcileAllSkipsUnavailableProject(t *testing.T) {
	store := newAssignmentStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &session.Project{Name: "Unavailable", CanonicalDir: root}
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	claimed, err := ReconcileAll(ctx, store)
	if err != nil || claimed != 0 {
		t.Fatalf("unavailable reconciliation = %d, %v", claimed, err)
	}
}

func TestReconcileAllResolvesEachWorkspaceOnceAcrossProjects(t *testing.T) {
	store := newAssignmentStore(t)
	ctx := context.Background()
	const projectCount = 4
	const workspaceCount = 13

	projects := make([]*session.Project, projectCount)
	for i := range projects {
		projects[i] = &session.Project{Name: fmt.Sprintf("Project %d", i), CanonicalDir: t.TempDir()}
		if err := store.CreateProject(ctx, projects[i]); err != nil {
			t.Fatal(err)
		}
	}

	projectByWorkspace := make(map[string]*session.Project, workspaceCount)
	sessionByWorkspace := make(map[string]string, workspaceCount)
	now := time.Now()
	for i := 0; i < workspaceCount; i++ {
		cwd := filepath.Join(t.TempDir(), fmt.Sprintf("persisted-workspace-%d", i))
		projectByWorkspace[cwd] = projects[i%projectCount]
		sessionID := fmt.Sprintf("legacy-workspace-%d", i)
		sessionByWorkspace[cwd] = sessionID
		sess := &session.Session{
			ID: sessionID, Provider: "mock", Model: "mock", CWD: cwd,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}

	resolverCalls := 0
	claimed, err := reconcileAll(ctx, store, func(_ context.Context, cwd, _ string) (workspaceIdentity, bool) {
		resolverCalls++
		p := projectByWorkspace[cwd]
		if p == nil {
			return workspaceIdentity{}, false
		}
		return workspaceIdentity{CanonicalDir: p.CanonicalDir}, true
	})
	if err != nil || claimed != workspaceCount {
		t.Fatalf("reconciliation = %d, %v; want %d", claimed, err, workspaceCount)
	}
	if resolverCalls != workspaceCount {
		t.Fatalf("workspace resolver calls = %d; want %d (one per workspace, not %d projects x %d workspaces)", resolverCalls, workspaceCount, projectCount, workspaceCount)
	}
	for cwd, wantProject := range projectByWorkspace {
		sess, err := store.Get(ctx, sessionByWorkspace[cwd])
		if err != nil {
			t.Fatal(err)
		}
		if sess.ProjectID != wantProject.ID {
			t.Fatalf("workspace %q project = %q; want %q", cwd, sess.ProjectID, wantProject.ID)
		}
	}
}

func TestAssignSessionForDirSkipsArchivedProject(t *testing.T) {
	store := newAssignmentStore(t)
	ctx := context.Background()
	root := t.TempDir()
	p := &session.Project{Name: "Archived", CanonicalDir: root}
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := store.UpdateProject(ctx, p.ID, session.ProjectUpdate{Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{CWD: root}
	matched, err := AssignSessionForDir(ctx, store, sess, root)
	if err != nil || matched != nil || sess.ProjectID != "" {
		t.Fatalf("archived assignment = project %#v session %#v, %v", matched, sess, err)
	}
}
