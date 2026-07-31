package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

type trackingAutotitleStore struct {
	*session.SQLiteStore
	updateCalls         int
	generatedTitleCalls int
}

func (s *trackingAutotitleStore) Update(ctx context.Context, sess *session.Session) error {
	s.updateCalls++
	return s.SQLiteStore.Update(ctx, sess)
}

func (s *trackingAutotitleStore) UpdateGeneratedTitle(ctx context.Context, id, shortTitle, longTitle string, generatedAt time.Time, basisMsgSeq int) error {
	s.generatedTitleCalls++
	return s.SQLiteStore.UpdateGeneratedTitle(ctx, id, shortTitle, longTitle, generatedAt, basisMsgSeq)
}

func TestUpdateAutotitleDoesNotClobberConcurrentSessionMetadata(t *testing.T) {
	ctx := context.Background()
	sqlStore, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = sqlStore.Close() })

	original := &session.Session{
		ID:        session.NewID(),
		Provider:  "test",
		Model:     "original-model",
		Mode:      session.ModeChat,
		Status:    session.StatusActive,
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	}
	if err := sqlStore.Create(ctx, original); err != nil {
		t.Fatalf("Create: %v", err)
	}

	stale, err := sqlStore.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("Get stale candidate: %v", err)
	}
	concurrent, err := sqlStore.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("Get concurrent session: %v", err)
	}
	concurrent.Name = "Manual title"
	concurrent.Pinned = true
	concurrent.Model = "new-model"
	concurrent.Status = session.StatusInterrupted
	concurrent.Goal = session.NewGoal("Preserve this goal", 1200, time.Now())
	concurrent.UpdatedAt = time.Now()
	if err := sqlStore.Update(ctx, concurrent); err != nil {
		t.Fatalf("concurrent Update: %v", err)
	}

	store := &trackingAutotitleStore{SQLiteStore: sqlStore}
	generatedAt := time.Now().UTC().Truncate(time.Second)
	if err := updateAutotitle(ctx, store, stale, "Generated short", "Generated long", generatedAt, 7); err != nil {
		t.Fatalf("updateAutotitle: %v", err)
	}

	if store.generatedTitleCalls != 1 {
		t.Fatalf("UpdateGeneratedTitle calls = %d, want 1", store.generatedTitleCalls)
	}
	if store.updateCalls != 0 {
		t.Fatalf("Update calls = %d, want 0", store.updateCalls)
	}

	loaded, err := sqlStore.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("Get updated session: %v", err)
	}
	if loaded.GeneratedShortTitle != "Generated short" || loaded.GeneratedLongTitle != "Generated long" || loaded.TitleSource != session.TitleSourceGenerated || loaded.TitleBasisMsgSeq != 7 {
		t.Fatalf("generated title fields not persisted: %#v", loaded)
	}
	if loaded.Name != concurrent.Name || loaded.Pinned != concurrent.Pinned || loaded.Model != concurrent.Model || loaded.Status != concurrent.Status {
		t.Fatalf("concurrent metadata clobbered: %#v", loaded)
	}
	if loaded.Goal == nil || loaded.Goal.Objective != concurrent.Goal.Objective || loaded.Goal.TokenBudget != concurrent.Goal.TokenBudget {
		t.Fatalf("concurrent goal clobbered: %#v", loaded.Goal)
	}
	if stale.GeneratedShortTitle != "Generated short" || stale.GeneratedLongTitle != "Generated long" || stale.TitleSource != session.TitleSourceGenerated || stale.TitleBasisMsgSeq != 7 || !stale.TitleGeneratedAt.Equal(generatedAt) {
		t.Fatalf("local title fields not updated after save: %#v", stale)
	}
}
