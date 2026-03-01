package cmd

import (
	"context"
	"path/filepath"
	"testing"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

func TestParseExtractionOperations_PlainJSON(t *testing.T) {
	raw := `{"operations": [{"op": "skip", "reason": "nothing to extract"}]}`
	ops, err := parseExtractionOperations(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops) != 1 || ops[0].Op != "skip" {
		t.Fatalf("unexpected ops: %+v", ops)
	}
}

func TestParseExtractionOperations_MarkdownFence(t *testing.T) {
	raw := "```json\n{\"operations\": [{\"op\": \"skip\", \"reason\": \"nothing to extract\"}]}\n```"
	ops, err := parseExtractionOperations(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops) != 1 || ops[0].Op != "skip" {
		t.Fatalf("unexpected ops: %+v", ops)
	}
}

func TestParseExtractionOperations_MarkdownFenceNoLang(t *testing.T) {
	raw := "```\n{\"operations\": [{\"op\": \"skip\", \"reason\": \"nothing\"}]}\n```"
	ops, err := parseExtractionOperations(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("unexpected ops: %+v", ops)
	}
}

func TestApplyExtractionOperations_AffectedPaths(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	agent := "jarvis"
	if err := store.CreateFragment(ctx, &memorydb.Fragment{
		Agent:   agent,
		Path:    "fragments/existing.md",
		Content: "old",
		Source:  memorydb.DefaultSourceMine,
	}); err != nil {
		t.Fatalf("CreateFragment(existing) error = %v", err)
	}

	ops := []extractionOperation{
		{Op: "create", Path: "fragments/new.md", Content: "new"},
		{Op: "update", Path: "fragments/existing.md", Content: "existing updated"},
		{Op: "update", Path: "fragments/missing.md", Content: "missing"},
		{Op: "skip", Reason: "no durable memory"},
		{Op: "create", Path: "fragments/dup.md", Content: "dup initial"},
		{Op: "update", Path: "fragments/dup.md", Content: "dup updated"},
	}

	oldDryRun := memoryDryRun
	memoryDryRun = false
	t.Cleanup(func() { memoryDryRun = oldDryRun })

	created, updated, skipped, affectedPaths, err := applyExtractionOperations(ctx, store, agent, ops)
	if err != nil {
		t.Fatalf("applyExtractionOperations() error = %v", err)
	}
	if created != 2 || updated != 2 || skipped != 2 {
		t.Fatalf("counts = create=%d update=%d skip=%d, want 2/2/2", created, updated, skipped)
	}

	want := map[string]bool{
		"fragments/new.md":      true,
		"fragments/existing.md": true,
		"fragments/dup.md":      true,
	}
	if len(affectedPaths) != len(want) {
		t.Fatalf("affectedPaths len = %d, want %d (%v)", len(affectedPaths), len(want), affectedPaths)
	}
	seen := map[string]int{}
	for _, p := range affectedPaths {
		seen[p]++
		if !want[p] {
			t.Fatalf("affectedPaths contains unexpected path %q (%v)", p, affectedPaths)
		}
	}
	for p := range want {
		if seen[p] != 1 {
			t.Fatalf("affected path %q count = %d, want 1 (%v)", p, seen[p], affectedPaths)
		}
	}

	memoryDryRun = true
	_, _, _, dryRunPaths, err := applyExtractionOperations(ctx, store, agent, []extractionOperation{
		{Op: "create", Path: "fragments/dry-run-create.md", Content: "x"},
		{Op: "update", Path: "fragments/existing.md", Content: "y"},
	})
	if err != nil {
		t.Fatalf("applyExtractionOperations(dry-run) error = %v", err)
	}
	if len(dryRunPaths) != 0 {
		t.Fatalf("affectedPaths in dry-run = %v, want empty", dryRunPaths)
	}
}
