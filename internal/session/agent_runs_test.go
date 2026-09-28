package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentRunStoreReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	ctx := context.Background()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	run := AgentRun{ID: "child", ParentSessionID: "parent", AgentName: "developer", Prompt: "work", Status: "running", OwnerInstanceID: "host:123:456", UpdatedAt: now, TurnsGranted: 20}
	if err := store.PutAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.GetAgentRun(ctx, "child")
	if err != nil || got.Status != "running" || got.ParentSessionID != "parent" {
		t.Fatalf("GetAgentRun = %+v, %v", got, err)
	}
	run.Status = "turn_limit"
	run.CollectedAt = now
	if err := store.PutAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListAgentRuns(ctx, "parent")
	if err != nil || len(list) != 1 || list[0].Status != "turn_limit" || list[0].CollectedAt.IsZero() {
		t.Fatalf("ListAgentRuns = %+v, %v", list, err)
	}
	other, err := store.ListAgentRuns(ctx, "other")
	if err != nil || len(other) != 0 {
		t.Fatalf("other parent = %+v, %v", other, err)
	}
}
