package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	memorydb "github.com/term-llm/term-llm/internal/memory"
)

func TestPromotionSelectionIncludesRecentlyEditedOlderFragments(t *testing.T) {
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	for _, frag := range []memorydb.Fragment{
		{Agent: "assistant", Path: "fragments/edited.md", Content: "new observation", CreatedAt: now.Add(-90 * 24 * time.Hour), UpdatedAt: now.Add(-time.Minute)},
		{Agent: "assistant", Path: "fragments/stale.md", Content: "old observation", CreatedAt: now.Add(-90 * 24 * time.Hour), UpdatedAt: now.Add(-60 * 24 * time.Hour)},
		{Agent: "other", Path: "fragments/other.md", Content: "other agent", CreatedAt: now.Add(-90 * 24 * time.Hour), UpdatedAt: now.Add(-time.Minute)},
	} {
		if err := store.CreateFragment(ctx, &frag); err != nil {
			t.Fatal(err)
		}
	}
	changed, _, err := collectPromoteChangedFragments(ctx, store, "assistant", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].Path != "fragments/edited.md" {
		t.Fatal("promotion omitted recently edited older record or included stale/other-agent records")
	}
	cutoff := now.Add(-time.Hour)
	// Listing's existing --since contract is about when a fact was learned.
	created, err := store.ListFragments(ctx, memorydb.ListOptions{Agent: "assistant", Since: &cutoff})
	if err != nil || len(created) != 0 {
		t.Fatal("creation-time listing semantics changed")
	}
}

func TestPromotionSelectionRespectsTimeZoneOffsets(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	for _, hours := range []int{-8, 11} {
		t.Run(fmt.Sprintf("UTC%+d", hours), func(t *testing.T) {
			store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			local := time.FixedZone("TST", hours*3600)
			for _, frag := range []memorydb.Fragment{
				{Agent: "assistant", Path: "fragments/recent.md", Content: "recent", CreatedAt: now.Add(-90 * 24 * time.Hour), UpdatedAt: now.Add(-time.Minute).In(local)},
				{Agent: "assistant", Path: "fragments/stale.md", Content: "stale", CreatedAt: now.Add(-90 * 24 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour).In(local)},
			} {
				if err := store.CreateFragment(ctx, &frag); err != nil {
					t.Fatal(err)
				}
			}
			changed, _, err := collectPromoteChangedFragments(ctx, store, "assistant", time.Hour, now)
			if err != nil || len(changed) != 1 || changed[0].Path != "fragments/recent.md" {
				t.Fatalf("time zone offset changed chronological selection: err=%v changed=%+v", err, changed)
			}
			// Exercise the real update path, which uses time.Now in the local zone.
			previousLocal := time.Local
			time.Local = local
			defer func() { time.Local = previousLocal }()
			if _, err := store.UpdateFragment(ctx, "assistant", "fragments/stale.md", "newly updated"); err != nil {
				t.Fatal(err)
			}
			changed, _, err = collectPromoteChangedFragments(ctx, store, "assistant", time.Hour, time.Now().UTC())
			if err != nil || len(changed) != 2 {
				t.Fatal("real local-zone update omitted")
			}
		})
	}
}
