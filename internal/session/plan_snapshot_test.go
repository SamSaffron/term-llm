package session

import (
	"context"
	"testing"

	planpkg "github.com/term-llm/term-llm/internal/plan"
)

func TestSQLitePlanSnapshotLifecycle(t *testing.T) {
	store, err := NewStore(Config{Enabled: true, Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "mock", Model: "mock", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	planStore, ok := store.(PlanSnapshotStore)
	if !ok {
		t.Fatal("SQLite store does not implement PlanSnapshotStore")
	}
	if got, version, err := planStore.LoadPlanSnapshot(ctx, sess.ID); err != nil || version != 0 || len(got.Plan) != 0 {
		t.Fatalf("initial LoadPlanSnapshot = %#v version=%d err=%v", got, version, err)
	}
	first := planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Inspect", Status: planpkg.StatusInProgress}}}
	version, err := planStore.SavePlanSnapshot(ctx, sess.ID, first)
	if err != nil || version != 1 {
		t.Fatalf("first SavePlanSnapshot version=%d err=%v", version, err)
	}
	second := planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Inspect", Status: planpkg.StatusCompleted}, {Step: "Test", Status: planpkg.StatusPending}}}
	version, err = planStore.SavePlanSnapshot(ctx, sess.ID, second)
	if err != nil || version != 2 {
		t.Fatalf("second SavePlanSnapshot version=%d err=%v", version, err)
	}
	got, version, err := planStore.LoadPlanSnapshot(ctx, sess.ID)
	if err != nil || version != 2 || !got.Equal(second) {
		t.Fatalf("LoadPlanSnapshot = %#v version=%d err=%v", got, version, err)
	}
	if err := planStore.DeletePlanSnapshot(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, version, err := planStore.LoadPlanSnapshot(ctx, sess.ID); err != nil || version != 0 {
		t.Fatalf("load after clear version=%d err=%v", version, err)
	}
}

func TestSQLitePlanSnapshotDiscardsInvalidStoredSnapshot(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "mock", Model: "mock", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO session_plans (session_id, snapshot, version)
		VALUES (?, ?, 3)
	`, sess.ID, `{"plan":[{"step":"broken","status":"unknown"}]}`); err != nil {
		t.Fatal(err)
	}

	snapshot, version, err := store.LoadPlanSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("LoadPlanSnapshot should recover from invalid stored state: %v", err)
	}
	if version != 0 || len(snapshot.Plan) != 0 {
		t.Fatalf("LoadPlanSnapshot = %#v version=%d, want empty", snapshot, version)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_plans WHERE session_id = ?`, sess.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("invalid plan row remains: %d", rows)
	}
}

func TestSQLiteConditionalPlanCleanupPreservesNewerSnapshot(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "mock", Model: "mock", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	newer := planpkg.Snapshot{Plan: []planpkg.Step{{Step: "New valid plan", Status: planpkg.StatusInProgress}}}
	raw, err := newer.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO session_plans (session_id, snapshot, version)
		VALUES (?, ?, 4)
	`, sess.ID, string(raw)); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.deletePlanSnapshotVersion(ctx, sess.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("cleanup for stale version deleted a newer snapshot")
	}
	got, version, err := store.LoadPlanSnapshot(ctx, sess.ID)
	if err != nil || version != 4 || !got.Equal(newer) {
		t.Fatalf("newer snapshot = %#v version=%d err=%v", got, version, err)
	}
}

func TestSQLitePlanSnapshotCascadesWithSessionDelete(t *testing.T) {
	store, err := NewStore(Config{Enabled: true, Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "mock", Model: "mock", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	planStore := store.(PlanSnapshotStore)
	if _, err := planStore.SavePlanSnapshot(ctx, sess.ID, planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Work", Status: planpkg.StatusPending}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, version, err := planStore.LoadPlanSnapshot(ctx, sess.ID); err != nil || version != 0 {
		t.Fatalf("cascaded load version=%d err=%v", version, err)
	}
}
