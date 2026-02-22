package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreFragmentCRUDAndSearch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")

	store, err := NewStore(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	frag := &Fragment{
		Agent:   "reviewer",
		Path:    "preferences/editor",
		Content: "User prefers concise answers and durable summaries.",
		Source:  DefaultSourceMine,
	}
	if err := store.CreateFragment(ctx, frag); err != nil {
		t.Fatalf("CreateFragment() error = %v", err)
	}

	got, err := store.GetFragment(ctx, "reviewer", "preferences/editor")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetFragment() returned nil fragment")
	}
	if got.Content != frag.Content {
		t.Fatalf("content = %q, want %q", got.Content, frag.Content)
	}

	results, err := store.SearchFragments(ctx, "durable", 10, "reviewer")
	if err != nil {
		t.Fatalf("SearchFragments() error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SearchFragments() returned no rows")
	}

	updated, err := store.UpdateFragment(ctx, "reviewer", "preferences/editor", "User prefers terse answers.")
	if err != nil {
		t.Fatalf("UpdateFragment() error = %v", err)
	}
	if !updated {
		t.Fatal("UpdateFragment() returned updated=false")
	}

	results, err = store.SearchFragments(ctx, "terse", 10, "reviewer")
	if err != nil {
		t.Fatalf("SearchFragments() after update error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SearchFragments() after update returned no rows")
	}
}

func TestStoreMiningState(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")

	store, err := NewStore(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	st := &MiningState{
		SessionID:       "sess-1",
		Agent:           "reviewer",
		LastMinedOffset: 42,
		MinedAt:         time.Now().UTC(),
	}
	if err := store.UpsertState(ctx, st); err != nil {
		t.Fatalf("UpsertState() error = %v", err)
	}

	got, err := store.GetState(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetState() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetState() returned nil")
	}
	if got.LastMinedOffset != 42 {
		t.Fatalf("offset = %d, want 42", got.LastMinedOffset)
	}

	st.LastMinedOffset = 100
	if err := store.UpsertState(ctx, st); err != nil {
		t.Fatalf("UpsertState(update) error = %v", err)
	}

	got, err = store.GetState(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetState(update) error = %v", err)
	}
	if got.LastMinedOffset != 100 {
		t.Fatalf("offset after update = %d, want 100", got.LastMinedOffset)
	}
}
