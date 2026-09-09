package session

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSessionInputRefreshTransactionContract(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, tools  string
		system               bool
		transcript, metadata int
	}{
		{"no-op", "old", "saved", true, 0, 0},
		{"prompt", "new", "saved", true, 1, 0},
		{"tools", "old", "new", true, 0, 1},
		{"both", "new", "new", true, 1, 1},
		{"empty", "", "saved", true, 1, 0},
		{"absent", "new", "saved", false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "test.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			sess := &Session{ID: NewID(), Tools: "saved", Search: true, MCP: "unchanged", Model: "model", Provider: "provider", Mode: ModeChat, LastTotalTokens: 123, LastMessageCount: 4}
			if err := store.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			if tc.system {
				if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, llm.SystemText("old"), 0)); err != nil {
					t.Fatal(err)
				}
			}
			for i, msg := range []llm.Message{llm.UserText("human"), llm.SystemText("later"), llm.AssistantText("reply")} {
				if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, msg, i+1)); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{"a", "b"} {
				if err := store.SaveProviderState(ctx, sess.ID, key, []byte("state")); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := store.GetMessages(ctx, sess.ID, 0, 0)
			saved, _ := store.Get(ctx, sess.ID)
			beforeRev, _ := store.TranscriptRev(ctx, sess.ID)
			var cursor int64
			if err := store.db.QueryRow(`SELECT COALESCE(MAX(sequence),0) FROM session_change_log`).Scan(&cursor); err != nil {
				t.Fatal(err)
			}
			result, err := store.RefreshSessionInputs(ctx, sess.ID, tc.prompt, tc.tools, func(a, b string) bool { return a == b })
			if err != nil {
				t.Fatal(err)
			}
			after, _ := store.GetMessages(ctx, sess.ID, 0, 0)
			for i := range before {
				if before[i].Role != llm.RoleSystem || before[i].Sequence != 0 {
					if !reflect.DeepEqual(before[i], after[i]) {
						t.Fatalf("non-owned row changed: %d", i)
					}
				}
			}
			updated, _ := store.Get(ctx, sess.ID)
			afterRev, _ := store.TranscriptRev(ctx, sess.ID)
			if afterRev-beforeRev != int64(tc.transcript) || updated.Tools != tc.tools || updated.Search != saved.Search || updated.MCP != saved.MCP || updated.LastTotalTokens != saved.LastTotalTokens || updated.LastMessageCount != saved.LastMessageCount {
				t.Fatalf("unexpected metadata: %+v", updated)
			}
			for kind, want := range map[string]int{"session.transcript_changed": tc.transcript, "session.metadata_changed": tc.metadata} {
				var count int
				if err := store.db.QueryRow(`SELECT COUNT(*) FROM session_change_log WHERE sequence>? AND kind=?`, cursor, kind).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Fatalf("%s events = %d want %d", kind, count, want)
				}
			}
			for _, key := range []string{"a", "b"} {
				blob, err := store.LoadProviderState(ctx, sess.ID, key)
				if err != nil {
					t.Fatal(err)
				}
				if result.Changed() && len(blob) != 0 {
					t.Fatal("stale provider state survived")
				}
				if !result.Changed() && string(blob) != "state" {
					t.Fatal("no-op deleted provider state")
				}
			}
			again, err := store.RefreshSessionInputs(ctx, sess.ID, tc.prompt, tc.tools, func(a, b string) bool { return a == b })
			if err != nil || again.Changed() {
				t.Fatalf("repeat was not a no-op: %+v %v", again, err)
			}
		})
	}
}

func TestSessionInputRefreshCompactionAndRollback(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess := &Session{ID: NewID(), Tools: "saved", CompactionCount: 1, CompactionSeq: 2}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for i, msg := range []llm.Message{llm.SystemText("initial"), llm.UserText("preserved"), llm.SystemText("active"), llm.SystemText("later"), llm.UserText("tail")} {
		if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, msg, i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE sessions SET compaction_seq=2,compaction_count=1 WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := store.GetMessages(ctx, sess.ID, 0, 0)
	if _, err := store.db.Exec(`CREATE TRIGGER reject_refresh BEFORE UPDATE OF tools ON sessions BEGIN SELECT RAISE(ABORT,'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshSessionInputs(ctx, sess.ID, "new", "new", nil); err == nil {
		t.Fatal("expected rollback")
	}
	rolledBack, _ := store.GetMessages(ctx, sess.ID, 0, 0)
	if !reflect.DeepEqual(before, rolledBack) {
		t.Fatal("partial prompt changes escaped rollback")
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_refresh`); err != nil {
		t.Fatal(err)
	}
	result, err := store.RefreshSessionInputs(ctx, sess.ID, "", "saved", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 || result.Messages[0].Sequence != 0 || result.Messages[1].Sequence != 2 {
		t.Fatalf("changed wrong rows: %+v", result)
	}
	after, _ := store.Get(ctx, sess.ID)
	if after.CompactionSeq != 2 || after.CompactionCount != 1 {
		t.Fatal("boundary changed")
	}
	active, err := LoadActiveMessages(ctx, store, after)
	if err != nil {
		t.Fatal(err)
	}
	projected := ProjectSelectedSessionPrompt(LLMActiveMessages(active, 0, ""), "")
	if len(projected) != 2 || projected[0].Parts[0].Text != "later" {
		t.Fatalf("empty leading projection dropped later system: %+v", projected)
	}
	if _, err := store.RefreshSessionInputs(ctx, "missing", "prompt", "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// A non-system active row must not cause the search to advance to a later
	// system message. A stale empty boundary falls back to the initial row.
	if _, err := store.db.Exec(`UPDATE sessions SET compaction_seq=1 WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	result, err = store.RefreshSessionInputs(ctx, sess.ID, "new", "saved", nil)
	if err != nil || len(result.Messages) != 1 || result.Messages[0].Sequence != 0 {
		t.Fatalf("non-leading system changed: %+v %v", result, err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET compaction_seq=100 WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	result, err = store.RefreshSessionInputs(ctx, sess.ID, "fallback", "saved", nil)
	if err != nil || len(result.Messages) != 1 || result.Messages[0].Sequence != 0 {
		t.Fatalf("stale boundary: %+v %v", result, err)
	}
}
