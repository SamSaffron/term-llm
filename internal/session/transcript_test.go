package session

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func newTranscriptTestStore(t *testing.T) (*SQLiteStore, *Session) {
	t.Helper()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return store, sess
}

func requireTranscriptRevIncrease(t *testing.T, store TranscriptIndexer, sessionID string, mutate func() error) int64 {
	t.Helper()
	ctx := context.Background()
	before, err := store.TranscriptRev(ctx, sessionID)
	if err != nil {
		t.Fatalf("TranscriptRev before: %v", err)
	}
	if err := mutate(); err != nil {
		t.Fatalf("mutate transcript: %v", err)
	}
	after, err := store.TranscriptRev(ctx, sessionID)
	if err != nil {
		t.Fatalf("TranscriptRev after: %v", err)
	}
	if after <= before {
		t.Fatalf("transcript rev did not increase: before=%d after=%d", before, after)
	}
	return after
}

func TestSQLiteStoreTranscriptRevisionCoversMessageWritePaths(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()

	auto := NewMessage(sess.ID, llm.UserText("auto"), -1)
	requireTranscriptRevIncrease(t, store, sess.ID, func() error {
		return store.AddMessage(ctx, sess.ID, auto)
	})

	explicit := NewMessage(sess.ID, llm.AssistantText("explicit"), 10)
	requireTranscriptRevIncrease(t, store, sess.ID, func() error {
		return store.AddMessage(ctx, sess.ID, explicit)
	})

	explicit.Parts = llm.AssistantText("updated").Parts
	explicit.TextContent = "updated"
	requireTranscriptRevIncrease(t, store, sess.ID, func() error {
		return store.UpdateMessage(ctx, sess.ID, explicit)
	})

	requireTranscriptRevIncrease(t, store, sess.ID, func() error {
		return store.PersistCompactionTailHints(ctx, sess.ID, []int64{auto.ID})
	})

	replacement := []Message{
		*NewMessage(sess.ID, llm.UserText("replacement"), 0),
		*NewMessage(sess.ID, llm.AssistantText("answer"), 1),
	}
	requireTranscriptRevIncrease(t, store, sess.ID, func() error {
		return store.ReplaceMessages(ctx, sess.ID, replacement)
	})

	compacted := []Message{
		*NewMessage(sess.ID, llm.UserText("summary"), -1),
		*NewMessage(sess.ID, llm.AssistantText("continuation"), -1),
	}
	requireTranscriptRevIncrease(t, store, sess.ID, func() error {
		return store.CompactMessages(ctx, sess.ID, compacted)
	})

	active := []Message{
		*NewMessage(sess.ID, llm.UserText("summary changed"), -1),
		*NewMessage(sess.ID, llm.AssistantText("continuation changed"), -1),
	}
	requireTranscriptRevIncrease(t, store, sess.ID, func() error {
		return store.ReplaceCompactedMessages(ctx, sess.ID, active)
	})

	requireTranscriptRevIncrease(t, store, sess.ID, func() error {
		return store.ClearCompactionBoundary(ctx, sess.ID)
	})
}

func TestSQLiteStoreTranscriptIndexAndBodiesUseDurableIdentity(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	messages := []*Message{
		NewMessage(sess.ID, llm.SystemText("hidden"), -1),
		NewMessage(sess.ID, llm.UserText("hello"), -1),
		NewMessage(sess.ID, llm.AssistantText("answer"), -1),
		NewMessage(sess.ID, llm.Message{Role: llm.RoleTool}, -1),
		NewMessage(sess.ID, llm.Message{Role: llm.RoleEvent}, -1),
	}
	for _, msg := range messages {
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	rev, items, err := store.GetTranscriptIndex(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetTranscriptIndex: %v", err)
	}
	if rev != int64(len(messages)) {
		t.Fatalf("rev=%d want %d", rev, len(messages))
	}
	if len(items) != 4 {
		t.Fatalf("items=%d want 4: %#v", len(items), items)
	}
	wantRoles := []string{"user", "assistant", "tool", "event"}
	for i, item := range items {
		if item.ID != messages[i+1].ID || item.Seq != messages[i+1].Sequence || item.Role != wantRoles[i] {
			t.Fatalf("item[%d]=%#v", i, item)
		}
	}
	if items[2].Flags&TranscriptFlagEmptyBody == 0 || items[3].Flags&TranscriptFlagEmptyBody == 0 {
		t.Fatalf("empty rows lack empty-body flags: %#v", items)
	}

	bodyRev, bodies, err := store.GetMessagesByTranscriptRanges(ctx, sess.ID, []TranscriptRange{
		{StartSeq: items[2].Seq, StartID: items[2].ID, EndSeq: items[2].Seq, EndID: items[2].ID},
		{StartSeq: items[0].Seq, StartID: items[0].ID, EndSeq: items[0].Seq, EndID: items[0].ID},
	})
	if err != nil {
		t.Fatalf("GetMessagesByTranscriptRanges: %v", err)
	}
	if bodyRev != rev {
		t.Fatalf("body rev=%d index rev=%d", bodyRev, rev)
	}
	if len(bodies) != 2 || bodies[0].ID != messages[1].ID || bodies[1].ID != messages[3].ID {
		t.Fatalf("bodies not authoritative sequence order: %#v", bodies)
	}
}

func TestSQLiteStoreTranscriptIndexMatchesPlanResultDisplayVisibility(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	callID := "call-plan"
	messages := []*Message{
		NewMessage(sess.ID, llm.UserText("make a plan"), -1),
		NewMessage(sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
			Type:     llm.PartToolCall,
			ToolCall: &llm.ToolCall{ID: callID, Name: "update_plan"},
		}}}, -1),
		NewMessage(sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{
			Type:       llm.PartToolResult,
			ToolResult: &llm.ToolResult{ID: callID},
		}}}, -1),
	}
	for _, msg := range messages {
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	_, items, err := store.GetTranscriptIndex(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetTranscriptIndex: %v", err)
	}
	if got := items[2].Flags & TranscriptFlagEmptyBody; got != 0 {
		t.Fatalf("inferred update_plan result flagged empty: flags=%d", items[2].Flags)
	}
}

func TestSQLiteStoreTranscriptSnapshotIsCoherentDuringConcurrentWrites(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	const writes = 80
	var wg sync.WaitGroup
	wg.Add(1)
	writerErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, llm.UserText(fmt.Sprintf("row-%d", i)), -1)); err != nil {
				writerErr <- err
				return
			}
		}
	}()

	for i := 0; i < writes; i++ {
		snapshot, err := store.GetTranscriptSnapshot(ctx, sess.ID)
		if err != nil {
			t.Fatalf("GetTranscriptSnapshot: %v", err)
		}
		if snapshot.Rev != int64(len(snapshot.Items)) {
			t.Fatalf("incoherent snapshot: rev=%d rows=%d", snapshot.Rev, len(snapshot.Items))
		}
	}
	wg.Wait()
	select {
	case err := <-writerErr:
		t.Fatalf("writer: %v", err)
	default:
	}
	final, err := store.GetTranscriptSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Rev != writes || len(final.Items) != writes {
		t.Fatalf("final snapshot rev=%d rows=%d want=%d", final.Rev, len(final.Items), writes)
	}
}

func TestSQLiteStoreTranscriptRewriteRetiresIDs(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	original := []Message{
		*NewMessage(sess.ID, llm.UserText("one"), 0),
		*NewMessage(sess.ID, llm.AssistantText("two"), 1),
	}
	if err := store.ReplaceMessages(ctx, sess.ID, original); err != nil {
		t.Fatalf("ReplaceMessages original: %v", err)
	}
	_, before, err := store.GetTranscriptIndex(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldSecondID := before[1].ID

	rewrite := []Message{
		*NewMessage(sess.ID, llm.UserText("one"), 0),
		*NewMessage(sess.ID, llm.AssistantText("changed"), 1),
	}
	if err := store.ReplaceMessages(ctx, sess.ID, rewrite); err != nil {
		t.Fatalf("ReplaceMessages rewrite: %v", err)
	}
	_, after, err := store.GetTranscriptIndex(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].ID != before[0].ID {
		t.Fatalf("surviving prefix ID changed: before=%d after=%d", before[0].ID, after[0].ID)
	}
	if after[1].ID == oldSecondID {
		t.Fatalf("rewritten row retained retired ID %d", oldSecondID)
	}
}
