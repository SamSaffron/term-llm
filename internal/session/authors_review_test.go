package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
)

func TestAuthorImmutableOnMessageUpdate(t *testing.T) {
	store, sess := newBranchTestStore(t)
	ctx := context.Background()
	original := core.UserText("before")
	original.AuthorID = "alice"
	stored := addBranchTestMessage(t, store, sess.ID, original)
	for _, id := range []string{"", "bob"} {
		update := NewMessage(sess.ID, core.UserText("updated"), 0)
		update.ID = stored.ID
		update.AuthorID = id
		if err := store.UpdateMessage(ctx, sess.ID, update); err != nil {
			t.Fatal(err)
		}
		got, err := store.GetMessageByID(ctx, stored.ID)
		if err != nil || got.AuthorID != "alice" || got.TextContent != "updated" {
			t.Fatalf("update changed author: %+v %v", got, err)
		}
	}
}

func TestStarterAfterAnonymousFirstTurnUndone(t *testing.T) {
	store, sess := newBranchTestStore(t)
	ctx := context.Background()
	addBranchTestMessage(t, store, sess.ID, core.UserText("anonymous"))
	applyUndoRedo(t, store, sess.ID, false)
	user := core.UserText("identified replacement")
	user.AuthorID = "alice"
	addBranchTestMessage(t, store, sess.ID, user)
	got, err := store.Get(ctx, sess.ID)
	if err != nil || got.CreatedBy != "alice" {
		t.Fatalf("starter after undo=%+v %v", got, err)
	}
}

func TestStarterDoesNotDependOnUserTurnIncrementOrder(t *testing.T) {
	store, sess := newBranchTestStore(t)
	if _, err := store.db.Exec(`UPDATE sessions SET user_turns=7 WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	user := core.UserText("first row, count already incremented")
	user.AuthorID = "alice"
	addBranchTestMessage(t, store, sess.ID, user)
	got, err := store.Get(context.Background(), sess.ID)
	if err != nil || got.CreatedBy != "alice" {
		t.Fatalf("starter depended on counter: %+v %v", got, err)
	}
}

func TestAnonymousStarterRemainsUnknownAcrossRewrites(t *testing.T) {
	for _, writer := range []string{"replace", "reset", "compact", "replace compacted tail"} {
		t.Run(writer, func(t *testing.T) {
			store, sess := newBranchTestStore(t)
			ctx := context.Background()
			addBranchTestMessage(t, store, sess.ID, core.UserText("anonymous first"))
			user := core.UserText("later attributed user")
			user.AuthorID = "alice"
			addBranchTestMessage(t, store, sess.ID, user)
			rows, err := store.GetMessages(ctx, sess.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			switch writer {
			case "replace":
				err = store.ReplaceMessages(ctx, sess.ID, rows)
			case "reset":
				err = store.ResetMessages(ctx, sess.ID, rows)
			case "compact", "replace compacted tail":
				tail := []Message{*NewMessage(sess.ID, core.UserText("summary"), 0), *NewMessage(sess.ID, user, 1)}
				err = store.CompactMessages(ctx, sess.ID, tail)
				if err == nil && writer == "replace compacted tail" {
					err = store.ReplaceMessages(ctx, sess.ID, tail)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(ctx, sess.ID)
			if err != nil || got.CreatedBy != "" {
				t.Fatalf("rewrite assigned anonymous starter: %+v %v", got, err)
			}
		})
	}
}

func TestStoreStarterJSONUsesRawIDKey(t *testing.T) {
	for _, value := range []any{Session{CreatedBy: "alice"}, SessionSummary{CreatedBy: "alice"}, SearchResult{CreatedBy: "alice"}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["created_by"]; exists {
			t.Fatalf("raw ID collides with web author object: %s", data)
		}
		if fields["created_by_id"] != "alice" {
			t.Fatalf("raw starter lost: %s", data)
		}
	}
}
