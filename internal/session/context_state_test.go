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
