package cmd

import (
	"context"
	"errors"
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
	concurrent.TitleSource = session.TitleSourceUser
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
	if loaded.GeneratedShortTitle != "Generated short" || loaded.GeneratedLongTitle != "Generated long" || loaded.TitleBasisMsgSeq != 7 {
		t.Fatalf("generated title fields not persisted: %#v", loaded)
	}
	if loaded.TitleSource != session.TitleSourceUser {
		t.Fatalf("concurrent manual title source overwritten: got %q", loaded.TitleSource)
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

type recordingAutotitleStore struct {
	*session.NoopStore
	updated *session.Session
}

func (s *recordingAutotitleStore) Update(_ context.Context, sess *session.Session) error {
	copy := *sess
	s.updated = &copy
	return nil
}

func TestUpdateAutotitlePreservesManualTitleSourceInFallbackStore(t *testing.T) {
	sess := &session.Session{ID: session.NewID(), Name: "Manual title", TitleSource: session.TitleSourceUser}
	store := &recordingAutotitleStore{NoopStore: &session.NoopStore{}}
	generatedAt := time.Now().UTC()

	if err := updateAutotitle(context.Background(), store, sess, "generated short", "generated long", generatedAt, 3); err != nil {
		t.Fatalf("updateAutotitle: %v", err)
	}
	if store.updated == nil {
		t.Fatal("fallback store was not updated")
	}
	if store.updated.TitleSource != session.TitleSourceUser || sess.TitleSource != session.TitleSourceUser {
		t.Fatalf("manual title source changed: stored=%q in-memory=%q", store.updated.TitleSource, sess.TitleSource)
	}
	if store.updated.GeneratedShortTitle != "generated short" || sess.GeneratedShortTitle != "generated short" {
		t.Fatalf("generated title not updated: stored=%q in-memory=%q", store.updated.GeneratedShortTitle, sess.GeneratedShortTitle)
	}
}

type failingAutotitleStore struct {
	*session.NoopStore
}

func (s *failingAutotitleStore) Update(context.Context, *session.Session) error {
	return errors.New("save failed")
}

func TestUpdateAutotitleDoesNotMutateSessionOnSaveFailure(t *testing.T) {
	sess := &session.Session{ID: session.NewID(), GeneratedShortTitle: "old", TitleSource: session.TitleSourceUser}
	generatedAt := time.Now().UTC()

	err := updateAutotitle(context.Background(), &failingAutotitleStore{NoopStore: &session.NoopStore{}}, sess, "new short", "new long", generatedAt, 3)
	if err == nil {
		t.Fatal("updateAutotitle returned nil for failed save")
	}
	if sess.GeneratedShortTitle != "old" || sess.GeneratedLongTitle != "" || sess.TitleSource != session.TitleSourceUser || !sess.TitleGeneratedAt.IsZero() || sess.TitleBasisMsgSeq != 0 {
		t.Fatalf("session mutated after failed save: %#v", sess)
	}
}
