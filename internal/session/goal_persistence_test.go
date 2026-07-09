package session

import (
	"context"
	"testing"
	"time"
)

func TestSessionGoalPersistenceRoundTrip(t *testing.T) {
	store, err := NewSQLiteStore(Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	goal := NewGoal("port /goal", 5000, now)
	goal.TokensUsed = 123
	sess := &Session{
		ID:        "sess-goal",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      ModeChat,
		Origin:    OriginTUI,
		CreatedAt: now,
		UpdatedAt: now,
		Goal:      goal,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil || got.Goal == nil {
		t.Fatalf("Get() goal = nil")
	}
	if got.Goal.Objective != goal.Objective || got.Goal.TokenBudget != 5000 || got.Goal.TokensUsed != 123 || got.Goal.Status != GoalStatusActive {
		t.Fatalf("round-tripped goal = %+v", got.Goal)
	}

	updated := got.Goal.Clone()
	updated.Status = GoalStatusPaused
	updated.TokensUsed = 456
	if err := store.UpdateGoal(context.Background(), sess.ID, updated); err != nil {
		t.Fatalf("UpdateGoal() error = %v", err)
	}
	got, err = store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Get() after UpdateGoal error = %v", err)
	}
	if got.Goal == nil || got.Goal.Status != GoalStatusPaused || got.Goal.TokensUsed != 456 {
		t.Fatalf("updated goal = %+v", got.Goal)
	}

	list, err := store.List(context.Background(), ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Goal == nil || list[0].Goal.Status != GoalStatusPaused {
		t.Fatalf("list goal = %+v", list)
	}
}
