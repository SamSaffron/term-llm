package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
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

func TestBuildTaxonomyMap_RespectsBudget(t *testing.T) {
	fragments := make([]memorydb.Fragment, 0, 20)
	for i := 0; i < 20; i++ {
		fragments = append(fragments, memorydb.Fragment{
			Path:      filepath.ToSlash(filepath.Join("fragments", "prefs", "topic", time.Now().Format("150405"), strings.Repeat("x", 20)+".md")),
			UpdatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		})
	}
	got := buildTaxonomyMap(fragments, 60)
	if tokens := llm.EstimateTokens(got); tokens > 60 {
		t.Fatalf("taxonomy tokens = %d, want <= 60\n%s", tokens, got)
	}
	if !strings.Contains(got, "total_fragments") {
		t.Fatalf("taxonomy map missing summary: %s", got)
	}
}

func TestLoadMessagesForMining_RespectsPromptBudget(t *testing.T) {
	ctx := context.Background()
	sessStore, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer sessStore.Close()

	sess := &session.Session{
		ID:        session.NewID(),
		Provider:  "test",
		Model:     "test-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := sessStore.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 6; i++ {
		msg := session.NewMessage(sess.ID, llm.UserText(strings.Repeat("long durable text ", 80)), -1)
		if err := sessStore.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage(%d): %v", i, err)
		}
	}

	oldPromptMax := memoryMinePromptMaxTokens
	oldBatchSize := memoryMineBatchSize
	oldMaxMessages := memoryMineMaxMessages
	memoryMinePromptMaxTokens = 500
	memoryMineBatchSize = 10
	memoryMineMaxMessages = 0
	t.Cleanup(func() {
		memoryMinePromptMaxTokens = oldPromptMax
		memoryMineBatchSize = oldBatchSize
		memoryMineMaxMessages = oldMaxMessages
	})

	candidate := memoryMineCandidate{
		Summary: session.SessionSummary{Number: 1},
		Session: sess,
		Agent:   "jarvis",
	}
	messages, nextOffset, err := loadMessagesForMining(ctx, sessStore, candidate, 0, "Memory fragment map:\n- total_fragments: 0")
	if err != nil {
		t.Fatalf("loadMessagesForMining: %v", err)
	}
	if len(messages) == 0 {
		t.Fatal("expected at least one message")
	}
	if nextOffset >= 6 {
		t.Fatalf("nextOffset = %d, want partial batch due to prompt budget", nextOffset)
	}
	if got := estimateExtractionPromptTokens(candidate, 0, nextOffset, messages, "Memory fragment map:\n- total_fragments: 0"); got > memoryMinePromptMaxTokens {
		t.Fatalf("estimated prompt tokens = %d, want <= %d", got, memoryMinePromptMaxTokens)
	}
}
