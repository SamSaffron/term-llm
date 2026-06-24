package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func newContextStateTestStore(t *testing.T) Store {
	t.Helper()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedContextStateSession(t *testing.T, ctx context.Context, store Store) *Session {
	t.Helper()
	sess := &Session{ID: NewID(), Provider: "test", Model: "model", CreatedAt: time.Now(), UpdatedAt: time.Now(), CompactionSeq: -1, LastTotalTokens: 999, LastMessageCount: 42}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	msgs := []llm.Message{
		llm.SystemText("system"),
		llm.UserText("old user"),
		llm.AssistantText("old assistant"),
	}
	for _, msg := range msgs {
		if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	return sess
}

func TestLoadActiveMessagesUsesCompactionBoundary(t *testing.T) {
	ctx := context.Background()
	store := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, store)

	compactRows := []Message{*NewMessage(sess.ID, llm.UserText("summary"), -1), *NewMessage(sess.ID, llm.AssistantText("ack"), -1)}
	if err := store.CompactMessages(ctx, sess.ID, compactRows); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	refreshed, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	active, err := LoadActiveMessages(ctx, store, refreshed)
	if err != nil {
		t.Fatalf("LoadActiveMessages: %v", err)
	}
	if len(active) != 2 || active[0].TextContent != "summary" || active[1].TextContent != "ack" {
		t.Fatalf("active messages = %#v, want compacted rows only", active)
	}
}

func TestLoadActiveMessagesUsesLatestCompactionBoundary(t *testing.T) {
	ctx := context.Background()
	store := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, store)

	if err := store.CompactMessages(ctx, sess.ID, []Message{
		*NewMessage(sess.ID, llm.UserText("first summary"), -1),
		*NewMessage(sess.ID, llm.AssistantText("first ack"), -1),
	}); err != nil {
		t.Fatalf("first CompactMessages: %v", err)
	}
	first, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	firstSeq := first.CompactionSeq
	activeAfterFirst, err := LoadActiveMessages(ctx, store, first)
	if err != nil {
		t.Fatalf("LoadActiveMessages first: %v", err)
	}
	result := &llm.CompactionResult{NewMessages: []llm.Message{llm.UserText("second summary"), llm.AssistantText("second ack")}}
	if _, _, _, err := ApplyCompaction(ctx, store, first, activeAfterFirst, result); err != nil {
		t.Fatalf("second ApplyCompaction: %v", err)
	}
	second, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if second.CompactionCount != 2 {
		t.Fatalf("CompactionCount = %d, want 2", second.CompactionCount)
	}
	if second.CompactionSeq <= firstSeq {
		t.Fatalf("second CompactionSeq = %d, want greater than first %d", second.CompactionSeq, firstSeq)
	}
	active, err := LoadActiveMessages(ctx, store, second)
	if err != nil {
		t.Fatalf("LoadActiveMessages second: %v", err)
	}
	if len(active) != 2 || active[0].TextContent != "second summary" || active[1].TextContent != "second ack" {
		t.Fatalf("active after second compaction = %#v, want second compacted rows only", active)
	}
}

func TestLoadActiveMessagesUsesFullHistoryWithoutBoundary(t *testing.T) {
	ctx := context.Background()
	store := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, store)

	active, err := LoadActiveMessages(ctx, store, sess)
	if err != nil {
		t.Fatalf("LoadActiveMessages: %v", err)
	}
	if len(active) != 3 {
		t.Fatalf("len(active) = %d, want 3", len(active))
	}
}

func TestLoadScrollbackWithBoundary(t *testing.T) {
	ctx := context.Background()
	store := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, store)
	if err := store.CompactMessages(ctx, sess.ID, []Message{*NewMessage(sess.ID, llm.UserText("summary"), -1)}); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	refreshed, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	all, idx, err := LoadScrollbackWithBoundary(ctx, store, refreshed)
	if err != nil {
		t.Fatalf("LoadScrollbackWithBoundary: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("len(all) = %d, want 4", len(all))
	}
	if idx != 3 {
		t.Fatalf("idx = %d, want 3", idx)
	}
}

func TestLoadInitialScrollbackWithBoundaryStartsAtCompactionSeq(t *testing.T) {
	ctx := context.Background()
	base := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, base)
	if err := base.CompactMessages(ctx, sess.ID, []Message{
		*NewMessage(sess.ID, llm.UserText("summary"), -1),
		*NewMessage(sess.ID, llm.AssistantText("ack"), -1),
	}); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	refreshed, err := base.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	store := &trackingInitialScrollbackStore{Store: base}

	initial, idx, beforeSeq, err := LoadInitialScrollbackWithBoundary(ctx, store, refreshed)
	if err != nil {
		t.Fatalf("LoadInitialScrollbackWithBoundary: %v", err)
	}
	if len(initial) != 2 || initial[0].TextContent != "summary" || initial[1].TextContent != "ack" {
		t.Fatalf("initial scrollback = %#v, want summary/ack tail only", initial)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0 for active-only initial tail", idx)
	}
	if beforeSeq != refreshed.CompactionSeq {
		t.Fatalf("beforeSeq = %d, want compaction seq %d", beforeSeq, refreshed.CompactionSeq)
	}
	if store.getMessagesCalls != 0 {
		t.Fatalf("GetMessages calls = %d, want 0", store.getMessagesCalls)
	}
	if len(store.getMessagesFromCalls) != 1 || store.getMessagesFromCalls[0] != refreshed.CompactionSeq {
		t.Fatalf("GetMessagesFrom calls = %#v, want [%d]", store.getMessagesFromCalls, refreshed.CompactionSeq)
	}
}

func TestApplyCompactionPersistsAppendsRefreshesAndClearsEstimate(t *testing.T) {
	ctx := context.Background()
	store := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, store)
	full, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	result := &llm.CompactionResult{NewMessages: []llm.Message{llm.UserText("compact summary"), llm.AssistantText("ack")}}
	updated, activeStart, refreshed, err := ApplyCompaction(ctx, store, sess, full, result)
	if err != nil {
		t.Fatalf("ApplyCompaction: %v", err)
	}
	if activeStart != len(full) {
		t.Fatalf("activeStart = %d, want %d", activeStart, len(full))
	}
	if len(updated) != len(full)+2 {
		t.Fatalf("len(updated) = %d, want %d", len(updated), len(full)+2)
	}
	if refreshed == nil || refreshed.CompactionSeq < 0 || refreshed.CompactionCount != 1 {
		t.Fatalf("refreshed = %#v, want compaction metadata", refreshed)
	}
	if refreshed.LastTotalTokens != 0 || refreshed.LastMessageCount != 0 {
		t.Fatalf("context estimate = (%d,%d), want cleared", refreshed.LastTotalTokens, refreshed.LastMessageCount)
	}
	active, err := LoadActiveMessages(ctx, store, refreshed)
	if err != nil {
		t.Fatalf("LoadActiveMessages: %v", err)
	}
	if len(active) != 2 || active[0].TextContent != "compact summary" {
		t.Fatalf("persisted active = %#v, want compacted rows", active)
	}
}

func TestApplyCompactionDropsPersistedSystemAndReinjectsCurrentPrompt(t *testing.T) {
	ctx := context.Background()
	store := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, store)
	full, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	result := &llm.CompactionResult{NewMessages: []llm.Message{
		llm.SystemText("old compaction-time instructions"),
		llm.UserText("[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>"),
		llm.AssistantText("I've reviewed the context summary. I'll continue from where we left off."),
	}}
	updated, activeStart, refreshed, err := ApplyCompaction(ctx, store, sess, full, result)
	if err != nil {
		t.Fatalf("ApplyCompaction: %v", err)
	}
	if activeStart != len(full) {
		t.Fatalf("activeStart = %d, want %d", activeStart, len(full))
	}
	if len(updated) != len(full)+2 {
		t.Fatalf("len(updated) = %d, want old scrollback + summary/ack only", len(updated))
	}
	if strings.Contains(strings.Join(messageTexts(updated[activeStart:]), "\n"), "old compaction-time instructions") {
		t.Fatalf("compaction-time system prompt should not be persisted in compacted rows: %#v", updated[activeStart:])
	}

	active, err := LoadActiveMessages(ctx, store, refreshed)
	if err != nil {
		t.Fatalf("LoadActiveMessages: %v", err)
	}
	if len(active) != 2 || active[0].Role != llm.RoleUser || !llm.IsInternalCompactionSummaryText(active[0].TextContent) {
		t.Fatalf("persisted active rows = %#v, want summary + ack without pinned system", active)
	}
	llmActive := LLMActiveMessages(active, 0, "new current instructions")
	if len(llmActive) < 2 || llmActive[0].Role != llm.RoleSystem || llm.MessageText(llmActive[0]) != "new current instructions" {
		t.Fatalf("LLMActiveMessages did not inject current system prompt: %#v", llmActive)
	}
	if !llmActive[1].CacheAnchor {
		t.Fatalf("persisted compaction summary should rebuild with CacheAnchor=true: %#v", llmActive[1])
	}
}

func TestApplyCompactionMarksRetainedRawSuffixHiddenFromDisplay(t *testing.T) {
	full := []Message{
		*NewMessage("s", llm.UserText("old user"), 0),
		*NewMessage("s", llm.AssistantText("old assistant"), 1),
		*NewMessage("s", llm.UserText("recent user"), 2),
		*NewMessage("s", llm.AssistantText("recent assistant"), 3),
	}
	result := &llm.CompactionResult{NewMessages: []llm.Message{
		llm.UserText("[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>"),
		llm.UserText("recent user"),
		llm.AssistantText("recent assistant"),
	}}

	updated, activeStart, _, err := ApplyCompaction(context.Background(), nil, nil, full, result)
	if err != nil {
		t.Fatalf("ApplyCompaction: %v", err)
	}
	if activeStart != len(full) {
		t.Fatalf("activeStart = %d, want %d at compacted summary", activeStart, len(full))
	}
	got := messageTexts(updated)
	want := []string{"old user", "old assistant", "recent user", "recent assistant", "[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>", "recent user", "recent assistant"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("updated texts = %#v, want %#v", got, want)
	}
	if updated[2].CompactionTail || updated[3].CompactionTail || updated[4].CompactionTail {
		t.Fatalf("pre-compaction rows and summary should remain visible: %#v", updated)
	}
	if !updated[5].CompactionTail || !updated[6].CompactionTail {
		t.Fatalf("retained raw suffix should be marked hidden from display: %#v", updated[5:])
	}
}

func TestLoadScrollbackWithBoundaryMarksRetainedRawSuffixHiddenFromDisplay(t *testing.T) {
	ctx := context.Background()
	store := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, store)
	for _, msg := range []llm.Message{
		llm.UserText("recent user"),
		llm.AssistantText("recent assistant"),
	} {
		if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatalf("AddMessage recent: %v", err)
		}
	}
	if err := store.CompactMessages(ctx, sess.ID, []Message{
		*NewMessage(sess.ID, llm.UserText("[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>"), -1),
		*NewMessage(sess.ID, llm.AssistantText("I've reviewed the context summary. I'll continue from where we left off."), -1),
		*NewMessage(sess.ID, llm.UserText("recent user"), -1),
		*NewMessage(sess.ID, llm.AssistantText("recent assistant"), -1),
	}); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	refreshed, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	all, idx, err := LoadScrollbackWithBoundary(ctx, store, refreshed)
	if err != nil {
		t.Fatalf("LoadScrollbackWithBoundary: %v", err)
	}
	got := messageTexts(all)
	want := []string{"system", "old user", "old assistant", "recent user", "recent assistant", "[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>", "I've reviewed the context summary. I'll continue from where we left off.", "recent user", "recent assistant"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("scrollback texts = %#v, want %#v", got, want)
	}
	if idx != 5 {
		t.Fatalf("idx = %d, want 5 at compacted summary", idx)
	}
	if all[3].CompactionTail || all[4].CompactionTail || all[5].CompactionTail {
		t.Fatalf("pre-compaction retained rows and summary should remain visible: %#v", all)
	}
	if !all[6].CompactionTail || !all[7].CompactionTail || !all[8].CompactionTail {
		t.Fatalf("post-compaction ack/tail rows should be hidden from display: %#v", all[6:])
	}

	persisted, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after backfill: %v", err)
	}
	if !persisted[6].CompactionTail || !persisted[7].CompactionTail || !persisted[8].CompactionTail {
		t.Fatalf("legacy compaction tail hints were not persisted: %#v", persisted[6:])
	}

	active, err := LoadActiveMessages(ctx, store, refreshed)
	if err != nil {
		t.Fatalf("LoadActiveMessages: %v", err)
	}
	if len(active) != 4 || active[2].TextContent != "recent user" || active[3].TextContent != "recent assistant" {
		t.Fatalf("active messages = %#v, want compacted summary/ack plus retained raw suffix", active)
	}
}

func TestMarkCompactionDisplayTailsPersistsSyntheticAckWhenTailAlreadyMarked(t *testing.T) {
	summary := *NewMessage("s", llm.UserText("[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>"), 0)
	summary.ID = 1
	ack := *NewMessage("s", llm.AssistantText("I've reviewed the context summary. I'll continue from where we left off."), 1)
	ack.ID = 2
	tail := *NewMessage("s", llm.UserText("recent user"), 2)
	tail.ID = 3
	tail.CompactionTail = true
	messages := []Message{summary, ack, tail}

	marked, ids := markCompactionDisplayTailsForPersistence(messages)
	if !marked[1].CompactionTail {
		t.Fatalf("synthetic ack should be hidden when following tail row is already marked: %#v", marked)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("persisted IDs = %#v, want only synthetic ack ID 2", ids)
	}
}

func messageTexts(messages []Message) []string {
	out := make([]string, len(messages))
	for i, msg := range messages {
		out[i] = msg.TextContent
	}
	return out
}

type trackingInitialScrollbackStore struct {
	Store
	getMessagesCalls     int
	getMessagesFromCalls []int
}

func (s *trackingInitialScrollbackStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	s.getMessagesCalls++
	return s.Store.GetMessages(ctx, sessionID, limit, offset)
}

func (s *trackingInitialScrollbackStore) GetMessagesFrom(ctx context.Context, sessionID string, fromSeq, limit int) ([]Message, error) {
	s.getMessagesFromCalls = append(s.getMessagesFromCalls, fromSeq)
	return s.Store.GetMessagesFrom(ctx, sessionID, fromSeq, limit)
}

type failingAfterCompactStore struct {
	Store
	failUpdateContext bool
	failGet           bool
}

func (s failingAfterCompactStore) UpdateContextEstimate(ctx context.Context, id string, lastTotalTokens, lastMessageCount int) error {
	if s.failUpdateContext {
		return errors.New("forced context estimate failure")
	}
	return s.Store.UpdateContextEstimate(ctx, id, lastTotalTokens, lastMessageCount)
}

func (s failingAfterCompactStore) Get(ctx context.Context, id string) (*Session, error) {
	if s.failGet {
		return nil, errors.New("forced get failure")
	}
	return s.Store.Get(ctx, id)
}

func TestApplyCompactionReturnsUpdatedStateAfterBestEffortFailures(t *testing.T) {
	ctx := context.Background()
	base := newContextStateTestStore(t)
	sess := seedContextStateSession(t, ctx, base)
	full, err := base.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	store := failingAfterCompactStore{Store: base, failUpdateContext: true, failGet: true}

	result := &llm.CompactionResult{NewMessages: []llm.Message{llm.UserText("compact summary"), llm.AssistantText("ack")}}
	updated, activeStart, refreshed, err := ApplyCompaction(ctx, store, sess, full, result)
	if err != nil {
		t.Fatalf("ApplyCompaction returned error after CompactMessages succeeded: %v", err)
	}
	if activeStart != len(full) || len(updated) != len(full)+2 {
		t.Fatalf("updated state = len %d activeStart %d, want len %d activeStart %d", len(updated), activeStart, len(full)+2, len(full))
	}
	if refreshed != sess {
		t.Fatalf("refreshed = %#v, want original session when refresh fails", refreshed)
	}
	stored, err := base.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("base Get: %v", err)
	}
	if stored.CompactionSeq < 0 || stored.CompactionCount != 1 {
		t.Fatalf("stored compaction metadata = seq %d count %d, want compacted", stored.CompactionSeq, stored.CompactionCount)
	}
}

func TestLLMActiveMessagesExcludesPreBoundary(t *testing.T) {
	messages := []Message{
		*NewMessage("s", llm.UserText("old user"), 0),
		*NewMessage("s", llm.AssistantText("old assistant"), 1),
		*NewMessage("s", llm.UserText("summary"), 2),
		*NewMessage("s", llm.AssistantText("ack"), 3),
	}
	llmMsgs := LLMActiveMessages(messages, 2, "system")
	joined := ""
	for _, msg := range llmMsgs {
		for _, part := range msg.Parts {
			joined += part.Text + "\n"
		}
	}
	if strings.Contains(joined, "old user") || strings.Contains(joined, "old assistant") {
		t.Fatalf("LLMActiveMessages leaked pre-boundary rows: %q", joined)
	}
	if !strings.Contains(joined, "summary") || !strings.Contains(joined, "ack") {
		t.Fatalf("LLMActiveMessages missing active rows: %q", joined)
	}
}
