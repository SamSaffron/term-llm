package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentRunContinuationResetsCollectionAndWakeGeneration(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first := AgentRun{ID: "child", ParentSessionID: "parent", AgentName: "developer", Prompt: "work", Status: "completed", OwnerInstanceID: "host:1:1", RunGeneration: 1, NotifyWhenDone: true, NotifyOrigin: "web", UpdatedAt: time.Now()}
	if err := store.PutAgentRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.MarkAgentRunNotified(ctx, first.ID, 1, time.Now()); err != nil || !ok {
		t.Fatalf("mark first generation: %t %v", ok, err)
	}
	if err := store.CollectAgentRun(ctx, first.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	first.Status = "queued"
	first.RunGeneration = 2
	if err := store.PutAgentRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAgentRun(ctx, first.ID)
	if err != nil || got.RunGeneration != 2 || !got.NotifiedAt.IsZero() || !got.CollectedAt.IsZero() {
		t.Fatalf("continuation stale delivery state: %+v %v", got, err)
	}
	if ok, err := store.MarkAgentRunNotified(ctx, first.ID, 1, time.Now()); err != nil || ok {
		t.Fatalf("stale generation acknowledged: %t %v", ok, err)
	}
	first.RunGeneration = 1
	first.Status = "completed"
	if err := store.PutAgentRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.RunGeneration = 2
	first.Status = "completed"
	if err := store.PutAgentRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.Status = "running"
	if err := store.PutAgentRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetAgentRun(ctx, first.ID)
	if err != nil || got.Status != "completed" || got.RunGeneration != 2 {
		t.Fatalf("stale persistence rewrote terminal run: %+v %v", got, err)
	}
}

func TestAsAgentRunStoreUnsupported(t *testing.T) {
	if got := AsAgentRunStore(NewLoggingStore(&NoopStore{}, nil)); got != nil {
		t.Fatalf("unsupported store gained persistence: %T", got)
	}
}

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
	if err := store.PutAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.CollectAgentRun(ctx, run.ID, now); err != nil {
		t.Fatal(err)
	}
	if current, err := store.GetAgentRun(ctx, run.ID); err != nil || current.Status != "turn_limit" || current.CollectedAt.IsZero() {
		t.Fatalf("collect changed state: %+v, %v", current, err)
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
