package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/viper"
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
	if key != "update_recent_exhausted_at_jarvis" {
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

func TestReadLastUpdatedRecentAtIgnoresLegacyCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// Older versions advanced this key even when the input cap left sessions
	// unprocessed. Ignoring it on upgrade lets per-session offsets safely drain
	// that backlog before the new exhausted-run checkpoint is established.
	if err := store.SetMeta(ctx, "last_update_recent_at_jarvis", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	ts, err := readLastUpdatedRecentAt(ctx, store, "jarvis")
	if err != nil {
		t.Fatalf("readLastUpdatedRecentAt: %v", err)
	}
	if !ts.IsZero() {
		t.Fatalf("legacy checkpoint returned %v, want zero", ts)
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

func TestCollectMemoryUpdateRecentInputOnlyLoadsUpdatedAgentSessions(t *testing.T) {
	ctx := context.Background()
	checkpoint := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	oldUpdatedAt := checkpoint.Add(-time.Hour)
	recentUpdatedAt := checkpoint.Add(time.Minute)

	summaries := make([]session.SessionSummary, 0, 1002)
	for i := 0; i < 1000; i++ {
		summaries = append(summaries, session.SessionSummary{
			ID:        "old-" + strconv.Itoa(i),
			Number:    int64(i + 1),
			Agent:     "jarvis",
			Status:    session.StatusComplete,
			UpdatedAt: oldUpdatedAt,
		})
	}
	summaries = append(summaries,
		session.SessionSummary{ID: "recent-jarvis", Number: 1001, Agent: "jarvis", Status: session.StatusComplete, UpdatedAt: recentUpdatedAt},
		session.SessionSummary{ID: "recent-other", Number: 1002, Agent: "other", Status: session.StatusComplete, UpdatedAt: recentUpdatedAt},
	)

	sessStore := &updateRecentCountingStore{
		summaries: summaries,
		messages: map[string][]session.Message{
			"recent-jarvis": {{Role: llm.RoleUser, TextContent: "new activity", Sequence: 0}},
			"recent-other":  {{Role: llm.RoleUser, TextContent: "other activity", Sequence: 0}},
		},
		messageCalls: map[string]int{},
	}
	memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer memStore.Close()

	input, err := collectMemoryUpdateRecentInput(ctx, memStore, sessStore, "jarvis", checkpoint)
	if err != nil {
		t.Fatalf("collectMemoryUpdateRecentInput: %v", err)
	}
	if !strings.Contains(input.Text, "new activity") || strings.Contains(input.Text, "other activity") {
		t.Fatalf("input text = %q, want only recent jarvis activity", input.Text)
	}
	if sessStore.listCalls != 6 {
		t.Fatalf("List calls = %d, want 6 paginated scans", sessStore.listCalls)
	}
	if sessStore.getCalls != 0 {
		t.Fatalf("Get calls = %d, want 0", sessStore.getCalls)
	}
	if len(sessStore.messageCalls) != 1 || sessStore.messageCalls["recent-jarvis"] != 1 {
		t.Fatalf("GetMessagesFrom calls = %#v, want only recent-jarvis", sessStore.messageCalls)
	}
	if sessStore.lastListOptions.Agent != "jarvis" || sessStore.lastListOptions.Status != session.StatusComplete {
		t.Fatalf("List options = %#v, want agent and status filters", sessStore.lastListOptions)
	}
}

func TestCollectMemoryUpdateRecentInputNoOpDoesNotLoadHistoricalSessions(t *testing.T) {
	ctx := context.Background()
	checkpoint := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	summaries := make([]session.SessionSummary, 1000)
	for i := range summaries {
		summaries[i] = session.SessionSummary{
			ID:        "old-" + strconv.Itoa(i),
			Number:    int64(i + 1),
			Agent:     "jarvis",
			Status:    session.StatusComplete,
			UpdatedAt: checkpoint.Add(-time.Hour),
		}
	}
	sessStore := &updateRecentCountingStore{
		summaries:    summaries,
		messageCalls: map[string]int{},
	}
	memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer memStore.Close()

	input, err := collectMemoryUpdateRecentInput(ctx, memStore, sessStore, "jarvis", checkpoint)
	if err != nil {
		t.Fatalf("collectMemoryUpdateRecentInput: %v", err)
	}
	if input.Text != "" || !input.Exhausted {
		t.Fatalf("input = %#v, want an exhausted no-op", input)
	}
	if sessStore.getCalls != 0 || len(sessStore.messageCalls) != 0 {
		t.Fatalf("historical point queries: Get=%d GetMessagesFrom=%#v", sessStore.getCalls, sessStore.messageCalls)
	}
}

func TestCollectMemoryUpdateRecentInputExhaustedAtInputCap(t *testing.T) {
	oldMax := memoryUpdateRecentMaxInputChars
	memoryUpdateRecentMaxInputChars = len("[Session #1 - completed]\nUser: activity")
	t.Cleanup(func() { memoryUpdateRecentMaxInputChars = oldMax })

	for _, tc := range []struct {
		name      string
		count     int
		exhausted bool
	}{
		{name: "more sessions remain", count: 2, exhausted: false},
		{name: "last session consumed", count: 1, exhausted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summaries := make([]session.SessionSummary, 0, tc.count)
			messages := make(map[string][]session.Message, tc.count)
			for i := 0; i < tc.count; i++ {
				id := "session-" + strconv.Itoa(i+1)
				summaries = append(summaries, session.SessionSummary{
					ID: id, Number: int64(i + 1), Agent: "jarvis", Status: session.StatusComplete,
					UpdatedAt: time.Date(2026, 8, 23, 10, i, 0, 0, time.UTC),
				})
				messages[id] = []session.Message{{Role: llm.RoleUser, TextContent: "activity", Sequence: 0}}
			}
			sessStore := &updateRecentCountingStore{
				summaries: summaries, messages: messages, messageCalls: map[string]int{},
			}
			memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			defer memStore.Close()

			input, err := collectMemoryUpdateRecentInput(context.Background(), memStore, sessStore, "jarvis", time.Time{})
			if err != nil {
				t.Fatalf("collectMemoryUpdateRecentInput: %v", err)
			}
			if input.Exhausted != tc.exhausted {
				t.Fatalf("Exhausted = %v, want %v", input.Exhausted, tc.exhausted)
			}
			if len(sessStore.messageCalls) != 1 {
				t.Fatalf("GetMessagesFrom calls = %#v, want one capped session", sessStore.messageCalls)
			}
		})
	}
}

func TestCollectMemoryUpdateRecentInputPagesOversizedSession(t *testing.T) {
	oldMax := memoryUpdateRecentMaxInputChars
	memoryUpdateRecentMaxInputChars = 12000
	t.Cleanup(func() { memoryUpdateRecentMaxInputChars = oldMax })
	ctx := context.Background()
	memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer memStore.Close()
	messages := make([]session.Message, 1000)
	for i := range messages {
		role := llm.RoleUser
		if i%3 == 1 {
			role = llm.RoleAssistant
		}
		if i%3 == 2 {
			role = llm.RoleTool
		}
		messages[i] = session.Message{Role: role, TextContent: "message-" + strconv.Itoa(i) + ":" + strings.Repeat("x", 1800), Sequence: i * 2}
	}
	sessStore := &updateRecentCountingStore{
		summaries: []session.SessionSummary{{ID: "long", Number: 1, Agent: "jarvis", Status: session.StatusComplete}},
		messages:  map[string][]session.Message{"long": messages}, messageCalls: map[string]int{},
	}
	offset := 0
	for batch := 0; batch < len(messages); batch++ {
		calls := sessStore.messageCalls["long"]
		input, err := collectMemoryUpdateRecentInput(ctx, memStore, sessStore, "jarvis", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		for _, limit := range sessStore.messageLimits {
			if limit <= 0 || limit > 100 {
				t.Fatalf("unbounded page read: limit %d", limit)
			}
		}
		if len(input.Text) > memoryUpdateRecentMaxInputChars {
			t.Fatalf("oversized input: %d bytes > %d", len(input.Text), memoryUpdateRecentMaxInputChars)
		}
		next := input.Offsets["long"]
		if next <= offset {
			t.Fatalf("no progress: offset %d -> %d", offset, next)
		}
		if batch == 0 && (input.Exhausted || next >= messages[len(messages)-1].Sequence+1) {
			t.Fatal("first batch consumed oversized session")
		}
		if sessStore.messageCalls["long"]-calls > 2 {
			t.Fatal("read too many pages for one batch")
		}
		for _, msg := range messages {
			marker := "message-" + strconv.Itoa(msg.Sequence/2) + ":"
			want := msg.Sequence >= offset && msg.Sequence < next && msg.Role != llm.RoleTool
			if strings.Contains(input.Text, marker) != want {
				t.Fatalf("batch %d: incorrect inclusion of %s (offsets %d..%d)", batch, marker, offset, next)
			}
		}
		if err := memStore.SetMeta(ctx, updateRecentOffsetMetaKey("long"), strconv.Itoa(next)); err != nil {
			t.Fatal(err)
		}
		offset = next
		if input.Exhausted {
			if offset != messages[len(messages)-1].Sequence+1 {
				t.Fatalf("premature exhaustion at %d", offset)
			}
			return
		}
	}
	t.Fatal("never exhausted session")
}

func TestCollectMemoryUpdateRecentInputSingleMessageBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		budget    int
		wantError bool
	}{
		{"truncate UTF-8", 101, false},
		{"header cannot fit", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldMax := memoryUpdateRecentMaxInputChars
			memoryUpdateRecentMaxInputChars = tc.budget
			t.Cleanup(func() { memoryUpdateRecentMaxInputChars = oldMax })
			ctx := context.Background()
			memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer memStore.Close()
			sessStore := &updateRecentCountingStore{
				summaries:    []session.SessionSummary{{ID: "long", Number: 1, Agent: "jarvis", Status: session.StatusComplete}},
				messages:     map[string][]session.Message{"long": {{Role: llm.RoleAssistant, TextContent: strings.Repeat("界", 2000), Sequence: 7}}},
				messageCalls: map[string]int{},
			}
			input, err := collectMemoryUpdateRecentInput(ctx, memStore, sessStore, "jarvis", time.Time{})
			if tc.wantError {
				if err == nil {
					t.Fatal("expected budget error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(input.Text) > tc.budget || !utf8.ValidString(input.Text) || !strings.Contains(input.Text, "Assistant: 界") {
				t.Fatalf("invalid bounded input: %q", input.Text)
			}
			if input.Offsets["long"] != 8 || !input.Exhausted {
				t.Fatalf("no progress: %#v", input)
			}
			offset, err := readUpdateRecentOffset(ctx, memStore, "long")
			if err != nil || offset != 0 {
				t.Fatalf("collector persisted offset before successful generation: %d, %v", offset, err)
			}
		})
	}
}

func TestCollectMemoryUpdateRecentInputAcrossMessagePages(t *testing.T) {
	ctx := context.Background()
	memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer memStore.Close()
	messages := make([]session.Message, 301)
	for i := range messages {
		messages[i] = session.Message{Role: llm.RoleTool, Sequence: i}
	}
	messages[100].Role = llm.RoleUser
	messages[100].TextContent = "first page boundary"
	messages[200].Role = llm.RoleAssistant
	messages[200].TextContent = "second page boundary"
	sessStore := &updateRecentCountingStore{
		summaries: []session.SessionSummary{{ID: "paged", Number: 1, Agent: "jarvis", Status: session.StatusComplete}},
		messages:  map[string][]session.Message{"paged": messages}, messageCalls: map[string]int{},
	}
	input, err := collectMemoryUpdateRecentInput(ctx, memStore, sessStore, "jarvis", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := formatUpdateRecentSessionBlock(memoryUpdateRecentSession{Number: 1, Status: session.StatusComplete}, messages)
	if input.Text != want || !input.Exhausted || input.Offsets["paged"] != 301 {
		t.Fatalf("incorrect paged input: %#v", input)
	}
	if sessStore.messageCalls["paged"] != 4 {
		t.Fatalf("page reads = %d, want 4", sessStore.messageCalls["paged"])
	}
}

type updateRecentCountingStore struct {
	session.NoopStore
	summaries       []session.SessionSummary
	messages        map[string][]session.Message
	listCalls       int
	getCalls        int
	messageCalls    map[string]int
	messageLimits   []int
	lastListOptions session.ListOptions
}

func (s *updateRecentCountingStore) List(_ context.Context, opts session.ListOptions) ([]session.SessionSummary, error) {
	s.listCalls++
	s.lastListOptions = opts
	result := make([]session.SessionSummary, 0)
	for _, summary := range s.summaries {
		if opts.Status != "" && summary.Status != opts.Status {
			continue
		}
		if opts.Agent != "" && sessionAgentOrDefault(summary.Agent) != strings.TrimSpace(opts.Agent) {
			continue
		}
		if opts.BeforeNumber > 0 && summary.Number >= opts.BeforeNumber {
			continue
		}
		result = append(result, summary)
	}
	if opts.SortByNumberDesc {
		sort.Slice(result, func(i, j int) bool { return result[i].Number > result[j].Number })
	}
	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}
	return result, nil
}

func (s *updateRecentCountingStore) Get(_ context.Context, _ string) (*session.Session, error) {
	s.getCalls++
	return nil, nil
}

func (s *updateRecentCountingStore) GetMessagesFrom(_ context.Context, sessionID string, fromSeq, limit int) ([]session.Message, error) {
	s.messageCalls[sessionID]++
	s.messageLimits = append(s.messageLimits, limit)
	messages := s.messages[sessionID]
	result := make([]session.Message, 0, len(messages))
	for _, message := range messages {
		if message.Sequence >= fromSeq {
			result = append(result, message)
			if limit > 0 && len(result) == limit {
				break
			}
		}
	}
	return result, nil
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

func TestMemoryUpdateRecentRejectsEmptyOutputWithoutConsumingActivity(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, output := range []string{"reasoning-only", "whitespace-only"} {
			t.Run(fmt.Sprintf("compact=%v/%s", compact, output), func(t *testing.T) {
				viper.Reset()
				t.Cleanup(viper.Reset)
				ctx := context.Background()
				dir := t.TempDir()
				t.Setenv("HOME", dir)
				t.Setenv("XDG_CONFIG_HOME", dir)
				t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Content-Type", "text/event-stream")
					delta := map[string]string{"reasoning_content": "thinking without an answer"}
					if output == "whitespace-only" {
						delta = map[string]string{"content": " \t\n\u2003 "}
					}
					if compact && calls == 1 {
						delta = map[string]string{"content": strings.Repeat("oversized memory ", 100)}
					}
					data, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
					if err != nil {
						t.Error(err)
						return
					}
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
				}))
				defer server.Close()

				configDir := filepath.Join(dir, "term-llm")
				if err := os.MkdirAll(configDir, 0o755); err != nil {
					t.Fatal(err)
				}
				sessionPath := filepath.Join(dir, "sessions.db")
				cfg := fmt.Sprintf("default_provider: memory-test\nproviders:\n  memory-test:\n    type: openai_compatible\n    base_url: %s/v1\n    model: test-model\nsessions:\n  enabled: true\n  path: %s\n", server.URL, sessionPath)
				if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
					t.Fatal(err)
				}

				oldDB, oldAgent, oldFile, oldModel := memoryDBPath, memoryAgent, memoryUpdateRecentFile, memoryUpdateRecentModel
				oldDryRun, oldTarget := memoryDryRun, memoryUpdateRecentTargetTokens
				memoryDBPath, memoryAgent = filepath.Join(dir, "memory.db"), "jarvis"
				memoryUpdateRecentFile, memoryUpdateRecentModel = filepath.Join(dir, "recent.md"), "memory-test:test-model"
				memoryDryRun, memoryUpdateRecentTargetTokens = false, 100
				t.Cleanup(func() {
					memoryDBPath, memoryAgent, memoryUpdateRecentFile, memoryUpdateRecentModel = oldDB, oldAgent, oldFile, oldModel
					memoryDryRun, memoryUpdateRecentTargetTokens = oldDryRun, oldTarget
				})
				original := "## Current state\n\nKeep this working memory.\n"
				if err := os.WriteFile(memoryUpdateRecentFile, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
				sessStore, err := session.NewStore(session.Config{Enabled: true, Path: sessionPath})
				if err != nil {
					t.Fatal(err)
				}
				defer sessStore.Close()
				sess := &session.Session{ID: session.NewID(), Agent: "jarvis", Status: session.StatusComplete, CreatedAt: time.Now(), UpdatedAt: time.Now()}
				if err := sessStore.Create(ctx, sess); err != nil {
					t.Fatal(err)
				}
				for _, text := range []string{"already consumed", "new activity to remember"} {
					if err := sessStore.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText(text), -1)); err != nil {
						t.Fatal(err)
					}
				}
				store, err := openMemoryStore()
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				metadata := map[string]string{
					updateRecentOffsetMetaKey(sess.ID):  "1",
					memoryUpdateRecentMetaKey("jarvis"): time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
				}
				for key, value := range metadata {
					if err := store.SetMeta(ctx, key, value); err != nil {
						t.Fatal(err)
					}
				}
				err = runMemoryUpdateRecent(memoryUpdateRecentCmd, nil)
				if err == nil || !strings.Contains(err.Error(), "empty") {
					t.Errorf("update error = %v, want actionable empty-output error", err)
				}
				wantCalls := 1
				if compact {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Errorf("provider calls = %d, want %d", calls, wantCalls)
				}
				data, err := os.ReadFile(memoryUpdateRecentFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != original {
					t.Errorf("recent.md changed: got %q, want %q", data, original)
				}
				for key, want := range metadata {
					got, err := store.GetMeta(ctx, key)
					if err != nil || got != want {
						t.Errorf("metadata %s = %q, %v; want %q", key, got, err, want)
					}
				}
			})
		}
	}
}
