package session

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

// TestSQLiteStoreUpdateMessageOverwrites verifies that UpdateMessage replaces
// the content of an existing row without affecting sequence or created_at.
func TestSQLiteStoreUpdateMessageOverwrites(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := NewMessage(sess.ID, llm.Message{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{{Type: llm.PartText, Text: "initial draft"}},
	}, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	originalID := msg.ID
	if originalID == 0 {
		t.Fatal("AddMessage did not stamp msg.ID")
	}
	originalSeq := msg.Sequence
	originalCreated := msg.CreatedAt

	// Update content in place: same ID, different parts.
	msg.Parts = []llm.Part{
		{Type: llm.PartText, Text: "updated draft"},
		{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "some_tool"}},
	}
	msg.TextContent = "updated draft"
	if err := store.UpdateMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}

	msgs, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message after update, got %d", len(msgs))
	}
	got := msgs[0]
	if got.ID != originalID {
		t.Errorf("msg.ID changed: got %d, want %d", got.ID, originalID)
	}
	if got.Sequence != originalSeq {
		t.Errorf("msg.Sequence changed: got %d, want %d", got.Sequence, originalSeq)
	}
	if !got.CreatedAt.Equal(originalCreated) {
		t.Errorf("msg.CreatedAt changed: got %v, want %v", got.CreatedAt, originalCreated)
	}
	if len(got.Parts) != 2 {
		t.Fatalf("updated parts count = %d, want 2", len(got.Parts))
	}
	if got.Parts[0].Text != "updated draft" {
		t.Errorf("updated text = %q, want %q", got.Parts[0].Text, "updated draft")
	}
	if got.Parts[1].Type != llm.PartToolCall || got.Parts[1].ToolCall == nil || got.Parts[1].ToolCall.ID != "call-1" {
		t.Errorf("updated tool call part = %+v, want tool call with ID call-1", got.Parts[1])
	}
}

func TestSQLiteStoreUpdateMessageAdjustsVisibleMessageCountOnRoleChange(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := NewMessage(sess.ID, llm.Message{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{{Type: llm.PartText, Text: "visible"}},
	}, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	summaries, err := store.List(ctx, ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List before role change: %v", err)
	}
	if summaries[0].MessageCount != 1 {
		t.Fatalf("MessageCount before role change = %d, want 1", summaries[0].MessageCount)
	}

	msg.Role = llm.RoleTool
	msg.Parts = []llm.Part{{Type: llm.PartText, Text: "tool result"}}
	msg.TextContent = "tool result"
	if err := store.UpdateMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("UpdateMessage to tool: %v", err)
	}

	summaries, err = store.List(ctx, ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List after role change: %v", err)
	}
	if summaries[0].MessageCount != 0 {
		t.Fatalf("MessageCount after role change = %d, want 0", summaries[0].MessageCount)
	}
}

// TestSQLiteStoreUpdateMessageReturnsErrNotFound verifies that UpdateMessage
// targeting a missing row returns ErrNotFound (used for the
// compaction-race fallback path in the upsert callback).
func TestSQLiteStoreUpdateMessageReturnsErrNotFound(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Never-persisted ID.
	ghost := &Message{
		ID:        99999,
		SessionID: sess.ID,
		Role:      llm.RoleAssistant,
		Parts:     []llm.Part{{Type: llm.PartText, Text: "ghost"}},
	}
	err = store.UpdateMessage(ctx, sess.ID, ghost)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateMessage on missing ID: got %v, want ErrNotFound", err)
	}

	// Persisted row, but wrong session ID.
	msg := NewMessage(sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "real"}}}, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	err = store.UpdateMessage(ctx, "non-existent-session-id", msg)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateMessage on wrong session: got %v, want ErrNotFound", err)
	}
}

// TestSQLiteStoreUpdateMessageConcurrent verifies that UpdateMessage is safe
// under concurrent access. retryOnBusy wraps the operation, so contention
// should resolve to successful updates rather than SQLITE_BUSY failures.
func TestSQLiteStoreUpdateMessageConcurrent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := NewMessage(sess.ID, llm.Message{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{{Type: llm.PartText, Text: "seed"}},
	}, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	const workers = 8
	const iterations = 10

	var wg sync.WaitGroup
	errs := make(chan error, workers*iterations)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				update := &Message{
					ID:        msg.ID,
					SessionID: sess.ID,
					Role:      llm.RoleAssistant,
					Parts:     []llm.Part{{Type: llm.PartText, Text: "concurrent"}},
				}
				if err := store.UpdateMessage(ctx, sess.ID, update); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		// Any non-ErrNotFound error (including SQLITE_BUSY escaping) is a
		// regression — retryOnBusy should absorb contention.
		t.Errorf("concurrent UpdateMessage error: %v", err)
	}

	msgs, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message after concurrent updates, got %d", len(msgs))
	}
	if msgs[0].Parts[0].Text != "concurrent" {
		t.Errorf("final text = %q, want %q", msgs[0].Parts[0].Text, "concurrent")
	}
}
