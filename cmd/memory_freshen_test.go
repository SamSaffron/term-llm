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

// -- truncateUpdateRecentText --

func TestTruncateUpdateRecentText_ShortText(t *testing.T) {
	got := truncateUpdateRecentText("hello", 100, false)
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestTruncateUpdateRecentText_ExactLimit(t *testing.T) {
	got := truncateUpdateRecentText("abcde", 5, false)
	if got != "abcde" {
		t.Fatalf("got %q, want %q", got, "abcde")
	}
}

func TestTruncateUpdateRecentText_TruncatesNoEllipsis(t *testing.T) {
	got := truncateUpdateRecentText("abcdef", 5, false)
	if got != "abcde" {
		t.Fatalf("got %q, want %q", got, "abcde")
	}
}

func TestTruncateUpdateRecentText_TruncatesWithEllipsis(t *testing.T) {
	got := truncateUpdateRecentText("abcdef", 5, true)
	if got != "abcde..." {
		t.Fatalf("got %q, want %q", got, "abcde...")
	}
}

func TestTruncateUpdateRecentText_Empty(t *testing.T) {
	got := truncateUpdateRecentText("", 100, true)
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestTruncateUpdateRecentText_Whitespace(t *testing.T) {
	got := truncateUpdateRecentText("  hello  ", 100, false)
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

// -- updateRecentSessionState --

func TestUpdateRecentSessionState(t *testing.T) {
	cases := []struct {
		status session.SessionStatus
		want   string
	}{
		{session.StatusComplete, "completed"},
		{session.StatusActive, "active"},
		{session.StatusError, "error"},
		{session.StatusInterrupted, "interrupted"},
		{"", "unknown"},
		{"custom", "custom"},
	}
	for _, tc := range cases {
		got := updateRecentSessionState(tc.status)
		if got != tc.want {
			t.Errorf("updateRecentSessionState(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// -- meta keys --

func TestMemoryUpdateRecentMetaKey(t *testing.T) {
	key := memoryUpdateRecentMetaKey("jarvis")
	if key != "last_update_recent_at_jarvis" {
		t.Fatalf("got %q", key)
	}
}

func TestUpdateRecentOffsetMetaKey(t *testing.T) {
	key := updateRecentOffsetMetaKey("sess-123")
	if key != "update_recent_offset_sess-123" {
		t.Fatalf("got %q", key)
	}
}

// -- system prompts --

func TestMemoryUpdateRecentSystemPrompt_ContainsCurrentStateGuidance(t *testing.T) {
	prompt := memoryUpdateRecentSystemPrompt(4000, 16000)
	if !contains(prompt, "4000") {
		t.Error("prompt should mention target token count 4000")
	}
	if !contains(prompt, "16000") {
		t.Error("prompt should mention target char count 16000")
	}
	if !contains(prompt, "current-state working memory") {
		t.Error("prompt should describe recent.md as current-state working memory")
	}
	if !contains(prompt, "Replace superseded facts") {
		t.Error("prompt should instruct replacement of superseded facts")
	}
	if contains(prompt, "today's date section") {
		t.Error("prompt should no longer instruct dated append behaviour")
	}
}

func TestMemoryCompactRecentSystemPrompt_ContainsAggressiveCompactionGuidance(t *testing.T) {
	prompt := memoryCompactRecentSystemPrompt(4000, 16000)
	if !contains(prompt, "hard target") {
		t.Error("compact prompt should use a hard target")
	}
	if !contains(prompt, "Drop resolved, duplicated, stale") {
		t.Error("compact prompt should drop stale detail aggressively")
	}
	if !contains(prompt, "not a dated log or archive") {
		t.Error("compact prompt should reject dated log behaviour")
	}
}

func TestMemoryUpdateRecentUserPrompt(t *testing.T) {
	prompt := memoryUpdateRecentUserPrompt("snippets here", "existing content", "fragment facts")
	if !contains(prompt, "RECENT SESSION SNIPPETS") {
		t.Error("missing RECENT SESSION SNIPPETS label")
	}
	if !contains(prompt, "CURRENT RECENT MEMORY") {
		t.Error("missing CURRENT RECENT MEMORY label")
	}
	if !contains(prompt, "snippets here") {
		t.Error("missing snippets content")
	}
	if !contains(prompt, "existing content") {
		t.Error("missing existing content")
	}
	if !contains(prompt, "RECENT MEMORY FRAGMENTS") {
		t.Error("missing RECENT MEMORY FRAGMENTS label")
	}
	if !contains(prompt, "fragment facts") {
		t.Error("missing fragment content")
	}
}

func TestMemoryUpdateRecentUserPromptNoFragments(t *testing.T) {
	prompt := memoryUpdateRecentUserPrompt("snippets here", "existing content", "")
	if contains(prompt, "RECENT MEMORY FRAGMENTS") {
		t.Error("should omit RECENT MEMORY FRAGMENTS section when fragmentsText is empty")
	}
	if !contains(prompt, "RECENT SESSION SNIPPETS") {
		t.Error("missing RECENT SESSION SNIPPETS label")
	}
}

func TestMemoryCompactRecentUserPrompt(t *testing.T) {
	prompt := memoryCompactRecentUserPrompt("oversized memory")
	if !contains(prompt, "CANDIDATE RECENT MEMORY TO COMPACT") {
		t.Error("missing compact label")
	}
	if !contains(prompt, "oversized memory") {
		t.Error("missing candidate memory content")
	}
}

// -- high water mark calculation --

func TestHighWaterMarkCalculation(t *testing.T) {
	targetTokens := 4000
	targetChars := targetTokens * memoryUpdateRecentCharsPerToken   // 16000
	highWater := targetChars * memoryUpdateRecentHighWaterPct / 100 // 19200

	if targetChars != 16000 {
		t.Errorf("targetChars = %d, want 16000", targetChars)
	}
	if highWater != 19200 {
		t.Errorf("highWaterChars = %d, want 19200", highWater)
	}
	if highWater <= targetChars {
		t.Error("high water mark must be above target")
	}
}

func TestFitUpdatedRecentWithinBudget_ReturnsUnchangedWhenUnderHighWater(t *testing.T) {
	current := strings.Repeat("x", 100)
	got, err := fitUpdatedRecentWithinBudget(context.Background(), nil, "", current, 4000, 16000, 19200)
	if err != nil {
		t.Fatalf("fitUpdatedRecentWithinBudget: %v", err)
	}
	if got != current {
		t.Fatalf("got %q, want unchanged content", got)
	}
}

// -- formatUpdateRecentSessionBlock --

func TestFormatUpdateRecentSessionBlock_FiltersToolCalls(t *testing.T) {
	sess := memoryUpdateRecentSession{ID: "s1", Number: 42, Status: session.StatusComplete}
	messages := []session.Message{
		{Role: llm.RoleUser, TextContent: "hello"},
		{Role: llm.RoleAssistant, TextContent: "world"},
		{Role: "tool", TextContent: "should be skipped"},
	}
	block := formatUpdateRecentSessionBlock(sess, messages)
	if !contains(block, "User: hello") {
		t.Error("expected user message in block")
	}
	if !contains(block, "Assistant: world") {
		t.Error("expected assistant message in block")
	}
	if contains(block, "should be skipped") {
		t.Error("tool message should be filtered out")
	}
}

func TestFormatUpdateRecentSessionBlock_AssistantTruncated(t *testing.T) {
	sess := memoryUpdateRecentSession{ID: "s1", Number: 1, Status: session.StatusComplete}
	longText := string(make([]byte, memoryUpdateRecentAssistantCharCap+50))
	for i := range longText {
		longText = longText[:i] + "x" + longText[i+1:]
	}
	messages := []session.Message{
		{Role: llm.RoleAssistant, TextContent: longText},
	}
	block := formatUpdateRecentSessionBlock(sess, messages)
	if !contains(block, "...") {
		t.Error("long assistant text should be truncated with ellipsis")
	}
}

func TestFormatUpdateRecentSessionBlock_EmptyWhenNoRelevantMessages(t *testing.T) {
	sess := memoryUpdateRecentSession{ID: "s1", Number: 1, Status: session.StatusComplete}
	messages := []session.Message{
		{Role: "tool", TextContent: "tool only"},
	}
	block := formatUpdateRecentSessionBlock(sess, messages)
	if block != "" {
		t.Errorf("expected empty block, got %q", block)
	}
}

func TestFormatUpdateRecentSessionBlock_SessionHeader(t *testing.T) {
	sess := memoryUpdateRecentSession{ID: "s1", Number: 7, Status: session.StatusActive}
	messages := []session.Message{
		{Role: llm.RoleUser, TextContent: "hi"},
	}
	block := formatUpdateRecentSessionBlock(sess, messages)
	if !contains(block, "Session #7") {
		t.Error("expected session number in header")
	}
	if !contains(block, "active") {
		t.Error("expected active status in header")
	}
}

// -- meta key read/write via real DB --

func TestReadWriteUpdateRecentOffset(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// Missing key → 0
	offset, err := readUpdateRecentOffset(ctx, store, "sess-abc")
	if err != nil {
		t.Fatalf("readUpdateRecentOffset: %v", err)
	}
	if offset != 0 {
		t.Errorf("expected 0 for missing key, got %d", offset)
	}

	// Write and read back
	if err := store.SetMeta(ctx, updateRecentOffsetMetaKey("sess-abc"), "42"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	offset, err = readUpdateRecentOffset(ctx, store, "sess-abc")
	if err != nil {
		t.Fatalf("readUpdateRecentOffset: %v", err)
	}
	if offset != 42 {
		t.Errorf("expected 42, got %d", offset)
	}
}

func TestReadLastUpdatedRecentAt_Missing(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ts, err := readLastUpdatedRecentAt(ctx, store, "jarvis")
	if err != nil {
		t.Fatalf("readLastUpdatedRecentAt: %v", err)
	}
	if !ts.IsZero() {
		t.Errorf("expected zero time for missing key, got %v", ts)
	}
}

func TestReadLastUpdatedRecentAt_RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Second)
	if err := store.SetMeta(ctx, memoryUpdateRecentMetaKey("jarvis"), now.Format(time.RFC3339)); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	ts, err := readLastUpdatedRecentAt(ctx, store, "jarvis")
	if err != nil {
		t.Fatalf("readLastUpdatedRecentAt: %v", err)
	}
	if !ts.Equal(now) {
		t.Errorf("got %v, want %v", ts, now)
	}
}

// -- helpers --

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
