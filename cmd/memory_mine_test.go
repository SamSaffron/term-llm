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

func TestBuildInsightTranscriptWeightsUserTextOverAssistantAndSummarizesTools(t *testing.T) {
	messages := []session.Message{
		{Role: llm.RoleSystem, TextContent: strings.Repeat("system ", 100)},
		{Role: llm.RoleUser, TextContent: "fix it"},
		{
			Role:        llm.RoleAssistant,
			TextContent: strings.Repeat("assistant explanation with lots of irrelevant details ", 200),
			Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{
				Name:     "read_file",
				ToolInfo: "(memory_mine.go)",
			}}},
		},
		{
			Role:        llm.RoleTool,
			TextContent: strings.Repeat("TOOL_SENTINEL should never appear ", 200),
			Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{
				Name:    "read_file",
				Content: strings.Repeat("TOOL_SENTINEL should never appear ", 200),
			}}},
		},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("second assistant dump ", 200)},
		{Role: llm.RoleUser, TextContent: "no, mine the user discussion, not the bulky execution transcript"},
		{Role: llm.RoleAssistant, TextContent: strings.Repeat("third assistant dump ", 200)},
	}

	got := buildInsightTranscript(messages)
	var userTokens, nonUserTokens int
	var sawToolCall, sawToolResult bool
	for _, msg := range got {
		if msg.Role == string(llm.RoleSystem) {
			t.Fatalf("transcript included system message: %+v", msg)
		}
		if strings.Contains(msg.Text, "TOOL_SENTINEL") {
			t.Fatalf("transcript leaked tool output: %q", msg.Text)
		}
		if strings.Contains(msg.Text, "tool called: read_file") {
			sawToolCall = true
		}
		if strings.Contains(msg.Text, "tool result: read_file ok") {
			sawToolResult = true
		}
		switch msg.Role {
		case string(llm.RoleUser):
			userTokens += llm.EstimateTokens(msg.Text)
		default:
			nonUserTokens += llm.EstimateTokens(msg.Text)
		}
	}

	if userTokens == 0 {
		t.Fatalf("expected user tokens in transcript: %+v", got)
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("expected abbreviated tool call/result summaries, got %+v", got)
	}
	if nonUserTokens > userTokens {
		t.Fatalf("non-user tokens = %d, user tokens = %d; context must not dominate\n%+v", nonUserTokens, userTokens, got)
	}
}

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

func TestCollectInsightCandidatesAppliesLimitAfterSkippingMinedSessions(t *testing.T) {
	ctx := context.Background()
	sessStore, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer sessStore.Close()

	memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatalf("NewStore(memory): %v", err)
	}
	defer memStore.Close()

	oldLimit := memoryMineLimit
	oldAgent := memoryAgent
	oldSince := memoryMineSince
	oldIncludeSubagents := memoryMineIncludeSubagents
	memoryMineLimit = 2
	memoryAgent = "jarvis"
	memoryMineSince = 0
	memoryMineIncludeSubagents = false
	t.Cleanup(func() {
		memoryMineLimit = oldLimit
		memoryAgent = oldAgent
		memoryMineSince = oldSince
		memoryMineIncludeSubagents = oldIncludeSubagents
	})

	var complete []session.SessionSummary
	for i := 1; i <= 5; i++ {
		sess := &session.Session{
			ID:        session.NewID(),
			Provider:  "test",
			Model:     "test-model",
			Agent:     "jarvis",
			Status:    session.StatusComplete,
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
			UpdatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
		if err := sessStore.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%d): %v", i, err)
		}
		messageCount := 4
		if i == 3 {
			messageCount = 2
		}
		complete = append(complete, session.SessionSummary{
			ID:           sess.ID,
			Number:       sess.Number,
			MessageCount: messageCount,
			UpdatedAt:    sess.UpdatedAt,
		})

		if i <= 2 {
			if err := memStore.MarkInsightMined(ctx, sess.ID, "jarvis"); err != nil {
				t.Fatalf("MarkInsightMined(%d): %v", i, err)
			}
		}
	}

	got, err := collectInsightCandidates(ctx, sessStore, memStore, complete, "")
	if err != nil {
		t.Fatalf("collectInsightCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Session.ID != complete[3].ID || got[1].Session.ID != complete[4].ID {
		t.Fatalf("selected sessions = [%s %s], want [%s %s]", got[0].Session.ID, got[1].Session.ID, complete[3].ID, complete[4].ID)
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
