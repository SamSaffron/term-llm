package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkSQLiteStoreListWithLargeHistories(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "sessions.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
	if err != nil {
		b.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	const sessionCount = 120
	const messagesPerSession = 400
	base := time.Now().Add(-time.Duration(sessionCount) * time.Minute).UTC().Truncate(time.Second)
	seedSQLiteStoreListBenchmark(b, ctx, store, sessionCount, messagesPerSession, base)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		summaries, err := store.List(ctx, ListOptions{Limit: 50, SortByActivity: true})
		if err != nil {
			b.Fatalf("List: %v", err)
		}
		if len(summaries) != 50 {
			b.Fatalf("got %d summaries, want 50", len(summaries))
		}
	}
}

func BenchmarkNewSQLiteStoreExistingDB(b *testing.B) {
	dbPath := filepath.Join(b.TempDir(), "sessions.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
	if err != nil {
		b.Fatalf("NewSQLiteStore setup: %v", err)
	}
	if err := store.Close(); err != nil {
		b.Fatalf("Close setup store: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
		if err != nil {
			b.Fatalf("NewSQLiteStore: %v", err)
		}
		if err := store.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

func seedSQLiteStoreListBenchmark(b *testing.B, ctx context.Context, store *SQLiteStore, sessionCount, messagesPerSession int, base time.Time) {
	b.Helper()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	sessStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO sessions (id, number, name, summary, provider, model, mode, origin, created_at, updated_at, last_user_message_at, last_message_at, archived, pinned, status)
		VALUES (?, ?, '', '', 'test', 'test-model', 'chat', 'web', ?, ?, ?, ?, FALSE, FALSE, 'active')`)
	if err != nil {
		b.Fatalf("prepare session insert: %v", err)
	}
	defer sessStmt.Close()

	msgStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO messages (session_id, role, parts, text_content, created_at, sequence)
		VALUES (?, ?, '[]', 'hello', ?, ?)`)
	if err != nil {
		b.Fatalf("prepare message insert: %v", err)
	}
	defer msgStmt.Close()

	for i := 0; i < sessionCount; i++ {
		sessionID := fmt.Sprintf("sess-%03d", i)
		createdAt := base.Add(time.Duration(i) * time.Minute)
		lastVisibleAt := createdAt.Add(time.Duration(messagesPerSession-1) * time.Second)
		if _, err := sessStmt.ExecContext(ctx, sessionID, i+1, createdAt, lastVisibleAt, createdAt, lastVisibleAt); err != nil {
			b.Fatalf("insert session %d: %v", i, err)
		}
		for j := 0; j < messagesPerSession; j++ {
			role := "user"
			if j%2 == 1 {
				role = "assistant"
			}
			if j%10 == 0 {
				role = "tool"
			}
			if _, err := msgStmt.ExecContext(ctx, sessionID, role, createdAt.Add(time.Duration(j)*time.Second), j); err != nil {
				b.Fatalf("insert message %d/%d: %v", i, j, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("Commit: %v", err)
	}
}
