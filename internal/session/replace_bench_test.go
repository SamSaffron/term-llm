package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func BenchmarkSQLiteStoreReplaceMessagesLargeSnapshot(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "sessions.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
	if err != nil {
		b.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		b.Fatalf("Create: %v", err)
	}

	messages := make([]Message, 120)
	base := time.Now().UTC().Truncate(time.Second)
	for i := range messages {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		text := fmt.Sprintf("message %03d from benchmark", i)
		messages[i] = Message{
			SessionID:   sess.ID,
			Role:        role,
			Parts:       []llm.Part{{Type: llm.PartText, Text: text}},
			TextContent: text,
			DurationMs:  int64(i),
			CreatedAt:   base.Add(time.Duration(i) * time.Millisecond),
			Sequence:    i,
		}
	}

	if err := store.ReplaceMessages(ctx, sess.ID, messages); err != nil {
		b.Fatalf("seed ReplaceMessages: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.ReplaceMessages(ctx, sess.ID, messages); err != nil {
			b.Fatalf("ReplaceMessages: %v", err)
		}
	}
}
