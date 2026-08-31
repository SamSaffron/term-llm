package hub

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAttentionProjectionAtomicallyReplacesAndRetainsErrors(t *testing.T) {
	store, err := OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	first := []SessionActivity{{SessionID: "s1", Kind: "terminal_unseen", AttentionSeq: 2, TerminalAt: now}, {SessionID: "s2", Kind: "running", StartedAt: now}}
	if err := store.ReplaceNode(ctx, "node-a", "store-a", `"etag-1"`, first); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkError(ctx, "node-a", "offline"); err != nil {
		t.Fatal(err)
	}
	activities, syncs, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 2 || len(syncs) != 1 || syncs[0].LastError != "offline" {
		t.Fatalf("stale projection not retained: %+v %+v", activities, syncs)
	}
	if err := store.ReplaceNode(ctx, "node-a", "store-b", `"etag-2"`, []SessionActivity{{SessionID: "s9", Kind: "terminal_unseen", AttentionSeq: 7}}); err != nil {
		t.Fatal(err)
	}
	activities, syncs, err = store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 || activities[0].StoreInstanceID != "store-b" || activities[0].SessionID != "s9" || syncs[0].LastError != "node session store was replaced" {
		t.Fatalf("store replacement mixed projections or omitted diagnostic: %+v %+v", activities, syncs)
	}
}

func TestAttentionProjectionCapabilityLostKeepsRows(t *testing.T) {
	store, err := OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.ReplaceNode(ctx, "node-a", "store-a", "etag", []SessionActivity{{SessionID: "s1", Kind: "terminal_unseen"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnavailable(ctx, "node-a", true); err != nil {
		t.Fatal(err)
	}
	activities, syncs, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 || len(syncs) != 1 || syncs[0].Capability != AttentionLost {
		t.Fatalf("lost capability state = %+v %+v", activities, syncs)
	}
}
