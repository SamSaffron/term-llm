package session

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestSearchSortBeforeLimit(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Insertion/relevance order deliberately differs from last-message activity,
	// creation and metadata update times. Pins must not override search ordering.
	for index, id := range []string{"middle", "new-b", "old", "new-a", "archived"} {
		sess := &Session{ID: id, Provider: "test", Model: "test", Mode: ModeChat, Origin: OriginWeb, CreatedAt: base.Add(time.Duration(10-index) * time.Hour), UpdatedAt: base, Pinned: id == "middle", Archived: id == "archived"}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		if err := store.AddMessage(ctx, id, NewMessage(id, core.UserText("sortneedle match"), 0)); err != nil {
			t.Fatal(err)
		}
		activity := any(base.Add(5 * time.Hour))
		switch id {
		case "new-a", "new-b":
			activity = base.Add(9 * time.Hour)
		case "archived":
			activity = base.Add(20 * time.Hour)
		case "old":
			activity = nil // Falls back to creation; metadata update must not win.
		}
		created := base.Add(time.Hour)
		if id != "old" {
			created = base.Add(30 * time.Hour)
		}
		if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET last_message_at = ?, created_at = ?, updated_at = ? WHERE id = ?", activity, created, base.Add(100*time.Hour), id); err != nil {
			t.Fatal(err)
		}
	}
	// A second matching message must not consume a result slot in date order.
	if err := store.AddMessage(ctx, "middle", NewMessage("middle", core.UserText("sortneedle match"), 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET last_message_at = ? WHERE id = 'middle'", base.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		order    SearchSort
		limit    int
		archived bool
		want     []string
	}{
		{"default relevance", "", 1, false, []string{"middle"}},
		{"explicit relevance", SearchSortRelevance, 1, false, []string{"middle"}},
		{"newest before limit", SearchSortNewest, 1, false, []string{"new-a"}},
		{"oldest before limit", SearchSortOldest, 1, false, []string{"old"}},
		{"newest stable ties and distinct sessions", SearchSortNewest, 4, false, []string{"new-a", "new-b", "middle", "old"}},
		{"oldest stable ties", SearchSortOldest, 4, false, []string{"old", "middle", "new-a", "new-b"}},
		{"archived filter before limit", SearchSortNewest, 1, true, []string{"archived"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, err := store.Search(ctx, SearchOptions{Query: "sortneedle", Sort: tc.order, Limit: tc.limit, Archived: tc.archived, Categories: []string{"web"}})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, result := range results {
				ids = append(ids, result.SessionID)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("IDs = %v, want %v", ids, tc.want)
			}
		})
	}
	if _, err := store.Search(ctx, SearchOptions{Query: "sortneedle", Sort: "invalid"}); err == nil {
		t.Fatal("expected invalid sort error")
	}
	// Older read-only schemas have no last_message_at; keep their creation fallback.
	store.hasLastMessageAt = false
	results, err := store.Search(ctx, SearchOptions{Query: "sortneedle", Sort: SearchSortOldest, Limit: 1})
	if err != nil || len(results) != 1 || results[0].SessionID != "old" {
		t.Fatalf("legacy search = %#v, %v", results, err)
	}
}
