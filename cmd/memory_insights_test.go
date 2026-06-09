package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

func TestCountInsightMaintenancePreviews(t *testing.T) {
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	old := time.Now().Add(-60 * 24 * time.Hour).UTC()
	if err := store.CreateInsight(ctx, &memorydb.Insight{
		Agent:          "jarvis",
		Content:        "Old low-confidence insight",
		Category:       "workflow",
		Confidence:     0.2,
		CreatedAt:      old,
		LastReinforced: old,
	}); err != nil {
		t.Fatalf("CreateInsight old: %v", err)
	}
	if err := store.CreateInsight(ctx, &memorydb.Insight{
		Agent:      "jarvis",
		Content:    "Fresh high-confidence insight",
		Category:   "workflow",
		Confidence: 0.9,
	}); err != nil {
		t.Fatalf("CreateInsight fresh: %v", err)
	}

	decayCount, err := countInsightDecayPreview(ctx, store, "jarvis", 30)
	if err != nil {
		t.Fatalf("countInsightDecayPreview: %v", err)
	}
	if decayCount != 1 {
		t.Fatalf("decay preview count = %d, want 1", decayCount)
	}

	gcCount, err := countInsightGCCandidates(ctx, store, "jarvis", 0.3)
	if err != nil {
		t.Fatalf("countInsightGCCandidates: %v", err)
	}
	if gcCount != 1 {
		t.Fatalf("gc preview count = %d, want 1", gcCount)
	}

	insights, err := store.ListInsights(ctx, "jarvis", 0)
	if err != nil {
		t.Fatalf("ListInsights: %v", err)
	}
	if len(insights) != 2 {
		t.Fatalf("preview helpers mutated insights; count = %d, want 2", len(insights))
	}
}
