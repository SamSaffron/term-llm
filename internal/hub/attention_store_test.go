package hub

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
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
	first := []SessionActivity{
		{SessionID: "s1", SessionNumber: 101, Kind: "terminal_unseen", AttentionSeq: 2, TerminalAt: now},
		{SessionID: "s2", SessionNumber: 102, Kind: "running", StartedAt: now},
		{SessionID: "s3", SessionNumber: 103, Kind: "input_required", PendingInteractionCount: 2,
			PendingInteractionKinds: []string{"approval.workspace", "ask_user"}, InteractionRequiredSince: now},
	}
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
	if len(activities) != 3 || len(syncs) != 1 || syncs[0].LastError != "offline" {
		t.Fatalf("stale projection not retained: %+v %+v", activities, syncs)
	}
	if activities[0].Kind != "input_required" || activities[0].SessionNumber != 103 || activities[0].PendingInteractionCount != 2 || len(activities[0].PendingInteractionKinds) != 2 {
		t.Fatalf("input-required projection did not round trip: %+v", activities)
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

func TestAttentionProjectionRebuildInvalidatesCachedETag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE node_attention_sync (
 node_id TEXT PRIMARY KEY, store_instance_id TEXT NOT NULL DEFAULT '', etag TEXT NOT NULL DEFAULT '',
 capability_state TEXT NOT NULL DEFAULT 'unavailable', last_success_at INTEGER, last_error_at INTEGER,
 last_error TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL
);
CREATE TABLE hub_session_activity (
 node_id TEXT NOT NULL, store_instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('running','terminal_unseen')), response_id TEXT NOT NULL DEFAULT '',
 lifecycle_state TEXT NOT NULL DEFAULT '', attention_seq INTEGER NOT NULL DEFAULT 0,
 final_rev INTEGER NOT NULL DEFAULT 0, short_title TEXT NOT NULL DEFAULT '', long_title TEXT NOT NULL DEFAULT '',
 project_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT '', started_at INTEGER,
 terminal_at INTEGER, lease_expires_at INTEGER, observed_at INTEGER NOT NULL,
 PRIMARY KEY(node_id, store_instance_id, session_id, kind)
);
INSERT INTO node_attention_sync(node_id, store_instance_id, etag, capability_state, updated_at)
 VALUES('node-a', 'store-a', '"old-etag"', 'supported', 1);
INSERT INTO hub_session_activity(node_id, store_instance_id, session_id, kind, observed_at)
 VALUES('node-a', 'store-a', 's1', 'running', 1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenAttentionProjectionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	syncState, err := store.GetSync(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if syncState.ETag != "" {
		t.Fatalf("rebuilt projection retained stale validator %q", syncState.ETag)
	}
	activities, _, err := store.List(context.Background())
	if err != nil || len(activities) != 0 {
		t.Fatalf("rebuilt activities = %+v, %v", activities, err)
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

// TestAttentionProjectionRemoveSeenScopesByStoreSequenceAndKind pins the bulk
// clear's cache semantics: acknowledging one registration of a store removes
// that store's acknowledged rows everywhere, while newer completions, other
// sessions, other kinds, and other stores survive.
func TestAttentionProjectionRemoveSeenScopesByStoreSequenceAndKind(t *testing.T) {
	store, err := OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.ReplaceNode(ctx, "node-a", "store-a", "", []SessionActivity{
		{SessionID: "sess", Kind: "terminal_unseen", AttentionSeq: 5},
		{SessionID: "sess", Kind: "input_required", PendingInteractionCount: 1},
		{SessionID: "other", Kind: "terminal_unseen", AttentionSeq: 5},
	}); err != nil {
		t.Fatal(err)
	}
	// A second registration cached the same sequence; a third cached a newer
	// completion of the same session; a fourth watches another store.
	if err := store.ReplaceNode(ctx, "node-b", "store-a", "", []SessionActivity{{SessionID: "sess", Kind: "terminal_unseen", AttentionSeq: 5}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceNode(ctx, "node-c", "store-a", "", []SessionActivity{{SessionID: "sess", Kind: "terminal_unseen", AttentionSeq: 9}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceNode(ctx, "node-d", "store-b", "", []SessionActivity{{SessionID: "sess", Kind: "terminal_unseen", AttentionSeq: 5}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveSeen(ctx, "store-a", "sess", 5); err != nil {
		t.Fatal(err)
	}
	activities, _, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(activities))
	for _, activity := range activities {
		got = append(got, fmt.Sprintf("%s/%s/%s@%d", activity.NodeID, activity.SessionID, activity.Kind, activity.AttentionSeq))
	}
	sort.Strings(got)
	want := []string{
		"node-a/other/terminal_unseen@5",
		"node-a/sess/input_required@0",
		"node-c/sess/terminal_unseen@9",
		"node-d/sess/terminal_unseen@5",
	}
	if len(got) != len(want) {
		t.Fatalf("rows after RemoveSeen = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("rows after RemoveSeen = %v, want %v", got, want)
		}
	}
}
