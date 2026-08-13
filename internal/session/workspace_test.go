package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func createWorkspaceTestSession(t *testing.T, store *SQLiteStore, id string) *Session {
	t.Helper()
	now := time.Now()
	sess := &Session{ID: id, Provider: "mock", ProviderKey: "mock", Model: "mock-model", Mode: ModeChat, Origin: OriginTUI, CreatedAt: now, UpdatedAt: now, Status: StatusActive}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	return sess
}

func TestWorkspaceGrantMigration46AndCRUDCascade(t *testing.T) {
	if schemaVersion < 46 {
		t.Fatalf("schemaVersion = %d, want at least 46", schemaVersion)
	}
	foundMigration := false
	for _, migration := range migrations {
		if migration.version == 46 {
			foundMigration = true
		}
	}
	if !foundMigration {
		t.Fatal("migration 46 missing")
	}

	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	createWorkspaceTestSession(t, store, "workspace-session")
	now := time.Now().UTC().Truncate(time.Microsecond)
	grant := WorkspaceGrant{ID: "grant-1", Path: t.TempDir(), Access: WorkspaceAccessRead, Provenance: "guardian", Rationale: "reference", CreatedAt: now, UpdatedAt: now}
	if err := store.SaveWorkspaceGrant(ctx, "workspace-session", grant); err != nil {
		t.Fatal(err)
	}
	grants, err := store.ListWorkspaceGrants(ctx, "workspace-session")
	if err != nil || len(grants) != 1 {
		t.Fatalf("ListWorkspaceGrants = %#v, %v", grants, err)
	}
	if grants[0].ID != grant.ID || grants[0].Path != grant.Path || grants[0].Access != WorkspaceAccessRead || grants[0].Provenance != "guardian" || grants[0].Rationale != "reference" {
		t.Fatalf("restored grant = %#v", grants[0])
	}

	grant.Access = WorkspaceAccessWrite
	grant.Rationale = "write elevation"
	grant.UpdatedAt = now.Add(time.Second)
	if err := store.SaveWorkspaceGrant(ctx, "workspace-session", grant); err != nil {
		t.Fatal(err)
	}
	grants, err = store.ListWorkspaceGrants(ctx, "workspace-session")
	if err != nil || len(grants) != 1 || grants[0].Access != WorkspaceAccessWrite || grants[0].Rationale != "write elevation" {
		t.Fatalf("elevated grants = %#v, %v", grants, err)
	}

	if err := store.Delete(ctx, "workspace-session"); err != nil {
		t.Fatal(err)
	}
	grants, err = store.ListWorkspaceGrants(ctx, "workspace-session")
	if err != nil || len(grants) != 0 {
		t.Fatalf("grants after session cascade = %#v, %v", grants, err)
	}
}

func TestMigration46UpgradesVersion45Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DROP TABLE session_workspace_grants"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE schema_version SET version = 45"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err := upgraded.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	var table string
	if err := upgraded.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'session_workspace_grants'").Scan(&table); err != nil {
		t.Fatalf("workspace grants table missing after migration: %v", err)
	}
}

func TestWorkspaceGrantDeleteAndBranchInheritance(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sourceSession := createWorkspaceTestSession(t, store, "branch-source")
	primaryPath := t.TempDir()
	sourceSession.CWD = primaryPath
	if err := store.Update(ctx, sourceSession); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	primary := WorkspaceGrant{ID: "primary", Path: primaryPath, Access: WorkspaceAccessWrite, Provenance: "human-confirmed-primary", Rationale: "direct human confirmation", CreatedAt: now, UpdatedAt: now}
	grant := WorkspaceGrant{ID: "stable-id", Path: t.TempDir(), Access: WorkspaceAccessRead, Provenance: "guardian", Rationale: "compare source", CreatedAt: now.Add(time.Millisecond), UpdatedAt: now.Add(time.Millisecond)}
	legacyYolo := WorkspaceGrant{ID: "legacy-yolo", Path: t.TempDir(), Access: WorkspaceAccessWrite, Provenance: "yolo", Rationale: "legacy bug", CreatedAt: now.Add(2 * time.Millisecond), UpdatedAt: now.Add(2 * time.Millisecond)}
	for _, workspaceGrant := range []WorkspaceGrant{primary, grant, legacyYolo} {
		if err := store.SaveWorkspaceGrant(ctx, "branch-source", workspaceGrant); err != nil {
			t.Fatal(err)
		}
	}

	branch, err := store.CreateBranch(ctx, "branch-source", CreateBranchOptions{AnchorMessageID: 0})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := store.ListWorkspaceGrants(ctx, branch.Session.ID)
	if err != nil || len(copied) != 2 {
		t.Fatalf("branch grants = %#v, %v", copied, err)
	}
	byID := make(map[string]WorkspaceGrant, len(copied))
	for _, copiedGrant := range copied {
		byID[copiedGrant.ID] = copiedGrant
	}
	if _, copiedYolo := byID[legacyYolo.ID]; copiedYolo {
		t.Fatalf("branch copied legacy yolo authority: %#v", copied)
	}
	if byID[primary.ID].Path != primary.Path || byID[primary.ID].Provenance != primary.Provenance || byID[grant.ID].Path != grant.Path {
		t.Fatalf("branch grants = %#v, want matching primary confirmation and additional grant", copied)
	}
	if branch.Session.CWD != primaryPath {
		t.Fatalf("branch CWD = %q, want primary path %q", branch.Session.CWD, primaryPath)
	}

	if err := store.DeleteWorkspaceGrant(ctx, branch.Session.ID, grant.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorkspaceGrant(ctx, branch.Session.ID, grant.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	branchGrants, err := store.ListWorkspaceGrants(ctx, branch.Session.ID)
	if err != nil || len(branchGrants) != 1 || branchGrants[0].ID != primary.ID {
		t.Fatalf("branch primary after additional revoke = %#v, %v", branchGrants, err)
	}
	source, err := store.ListWorkspaceGrants(ctx, "branch-source")
	if err != nil || len(source) != 3 {
		t.Fatalf("source grants changed with branch revoke: %#v, %v", source, err)
	}
}
