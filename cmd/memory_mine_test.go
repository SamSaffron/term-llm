package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestValidateFragmentPath(t *testing.T) {
	for _, tc := range []struct {
		path    string
		want    string
		wantErr bool
	}{
		{"fragments/foo.md", "fragments/foo.md", false},
		{"fragments\\foo.md", "fragments/foo.md", false},
		{"../evil.md", "", true},
		{"/abs/path.md", "", true},
	} {
		got, err := validateFragmentPath(tc.path)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("validateFragmentPath(%q) expected error", tc.path)
			}
			continue
		}
		if err != nil {
			t.Fatalf("validateFragmentPath(%q) error = %v", tc.path, err)
		}
		if got != tc.want {
			t.Fatalf("validateFragmentPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestMemoryExtractionCollectorAndTools(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	collector := newMemoryExtractionCollector()
	createTool := &memoryCreateFragmentTool{store: store, agent: "jarvis", collector: collector}
	updateTool := &memoryUpdateFragmentTool{store: store, agent: "jarvis", collector: collector}

	oldDryRun := memoryDryRun
	memoryDryRun = false
	t.Cleanup(func() { memoryDryRun = oldDryRun })

	if _, err := createTool.Execute(ctx, json.RawMessage(`{"path":"fragments/new.md","content":"new content"}`)); err != nil {
		t.Fatalf("create tool error = %v", err)
	}
	if _, err := updateTool.Execute(ctx, json.RawMessage(`{"path":"fragments/new.md","content":"updated content"}`)); err != nil {
		t.Fatalf("update tool error = %v", err)
	}

	res := collector.result("done")
	if res.Created != 1 || res.Updated != 1 || res.Skipped != 0 {
		t.Fatalf("result counts = %+v, want create=1 update=1 skip=0", res)
	}
	if len(res.AffectedPaths) != 1 || res.AffectedPaths[0] != "fragments/new.md" {
		t.Fatalf("affected paths = %+v", res.AffectedPaths)
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
	loadResult, err := loadMessagesForMining(ctx, sessStore, candidate, 0, "Memory fragment map:\n- total_fragments: 0")
	if err != nil {
		t.Fatalf("loadMessagesForMining: %v", err)
	}
	if len(loadResult.Messages) == 0 {
		t.Fatal("expected at least one message")
	}
	if loadResult.NextOffset >= 6 {
		t.Fatalf("nextOffset = %d, want partial batch due to prompt budget", loadResult.NextOffset)
	}
	if got := estimateExtractionPromptTokens(candidate, 0, loadResult.NextOffset, loadResult.Messages, "Memory fragment map:\n- total_fragments: 0"); got > memoryMinePromptMaxTokens {
		t.Fatalf("estimated prompt tokens = %d, want <= %d", got, memoryMinePromptMaxTokens)
	}
}

func TestFitMessagesForPromptBudget_PrefersUserAndDurableAssistantText(t *testing.T) {
	oldPromptMax := memoryMinePromptMaxTokens
	memoryMinePromptMaxTokens = 1000
	t.Cleanup(func() { memoryMinePromptMaxTokens = oldPromptMax })

	candidate := memoryMineCandidate{
		Summary: session.SessionSummary{Number: 1},
		Session: &session.Session{ID: "sess-1"},
		Agent:   "jarvis",
	}
	messages := []session.Message{
		{Role: llm.RoleUser, TextContent: strings.Repeat("user durable preference ", 18)},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("ok sure maybe maybe ", 45)},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("Changed config path /etc/service and updated model budget summary. ", 18)},
	}

	fit, ok := fitMessagesForPromptBudget(candidate, 0, len(messages), messages, "Memory fragment map:\n- total_fragments: 0")
	if !ok {
		t.Fatal("expected fitMessagesForPromptBudget to succeed")
	}
	if got := estimateExtractionPromptTokens(candidate, 0, len(messages), fit.Messages, "Memory fragment map:\n- total_fragments: 0"); got > memoryMinePromptMaxTokens {
		t.Fatalf("estimated prompt tokens = %d, want <= %d", got, memoryMinePromptMaxTokens)
	}
	if fit.Messages[0].TextContent != messages[0].TextContent {
		t.Fatalf("user message should be preserved, got %q", fit.Messages[0].TextContent)
	}
	if utf8.RuneCountInString(fit.Messages[2].TextContent) <= utf8.RuneCountInString(fit.Messages[1].TextContent) {
		t.Fatalf("durable assistant text should retain more budget than filler: durable=%d filler=%d", utf8.RuneCountInString(fit.Messages[2].TextContent), utf8.RuneCountInString(fit.Messages[1].TextContent))
	}
	if fit.AssistantMessagesCut == 0 {
		t.Fatal("expected assistant truncation to occur")
	}
	if fit.UserMessagesCut != 0 {
		t.Fatalf("expected no user truncation, got %d", fit.UserMessagesCut)
	}
}
