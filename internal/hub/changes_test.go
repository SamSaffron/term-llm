package hub

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStoresBumpChangeFeed(t *testing.T) {
	feed := NewChangeFeed()
	ctx := context.Background()

	attention, err := OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer attention.Close()
	attention.Changes = feed
	if err := attention.ReplaceNode(ctx, "alpha", "store", "etag", nil); err != nil {
		t.Fatal(err)
	}
	if err := attention.MarkSuccess(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if got := feed.Version(); got != 1 {
		t.Fatalf("after attention writes version = %d, want 1 (MarkSuccess must not bump)", got)
	}

	delegations := NewDelegationStore(filepath.Join(t.TempDir(), "delegations.json"))
	delegations.Changes = feed
	if err := delegations.Add(Delegation{ID: "dlg_1", Status: DelegationStatusPending}); err != nil {
		t.Fatal(err)
	}
	if got := feed.Version(); got != 2 {
		t.Fatalf("after delegation add version = %d, want 2", got)
	}
}
