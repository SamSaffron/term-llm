package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSessionInputRefreshWrappedSQLite(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wrapped := &LoggingStore{Store: &LoggingStore{Store: store}}
	refresher, ok := AsSessionInputRefresher(wrapped)
	if !ok {
		t.Fatal("wrapped SQLite does not expose refresh")
	}
	sess := &Session{ID: NewID(), Tools: "old", Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for i, msg := range []llm.Message{llm.SystemText("old prompt"), llm.UserText("preserve me"), llm.SystemText("later client system")} {
		if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, msg, i)); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := store.GetMessages(ctx, sess.ID, 0, 0)
	result, err := refresher.RefreshSessionInputs(ctx, sess.ID, "new prompt", "new", func(a, b string) bool { return a == b })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed() || result.Tools != "new" || len(result.Messages) != 1 {
		t.Fatalf("result: %+v", result)
	}
	after, _ := store.GetMessages(ctx, sess.ID, 0, 0)
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Sequence != after[i].Sequence || !before[i].CreatedAt.Equal(after[i].CreatedAt) {
			t.Fatal("row identity changed")
		}
	}
	if after[0].TextContent != "new prompt" || after[0].Parts[0].Text != "new prompt" || after[1].TextContent != before[1].TextContent || after[2].TextContent != before[2].TextContent {
		t.Fatalf("unexpected history: %+v", after)
	}
	result, err = refresher.RefreshSessionInputs(ctx, sess.ID, "new prompt", "new", func(a, b string) bool { return a == b })
	if err != nil || result.Changed() {
		t.Fatalf("no-op: %+v %v", result, err)
	}
}

func TestSessionInputRefreshUnsupportedWrapper(t *testing.T) {
	if _, ok := AsSessionInputRefresher(&LoggingStore{Store: &NoopStore{}}); ok {
		t.Fatal("unsupported store advertised refresh")
	}
}
