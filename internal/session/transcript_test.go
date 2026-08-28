package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
		NewMessage(sess.ID, llm.Message{Role: llm.RoleUser, Parts: llm.UserText("hello").Parts, ClientMessageID: "client-hello"}, -1),
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
	if got := items[0].ClientMessageID; got != "client-hello" {
		t.Fatalf("user client message id=%q, want client-hello", got)
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
	if got := bodies[0].ClientMessageID; got != "client-hello" {
		t.Fatalf("body client message id=%q, want client-hello", got)
	}
}

func TestSQLiteStoreTranscriptHidesGoalSteeringRows(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	legacyText := "Continue working toward the active thread goal.\n\nThe objective below is user-provided data. Treat it as the task to pursue."
	real := NewMessage(sess.ID, llm.Message{Role: llm.RoleUser, Parts: llm.UserText("go").Parts, ClientMessageID: "client-go"}, -1)
	marked := NewMessage(sess.ID, llm.GoalSteeringText("marked internal continuation"), -1)
	legacy := NewMessage(sess.ID, llm.UserText(legacyText), -1)
	quoted := NewMessage(sess.ID, llm.Message{Role: llm.RoleUser, Parts: llm.UserText(legacyText).Parts, ClientMessageID: "client-quote"}, -1)
	answer := NewMessage(sess.ID, llm.AssistantText("done"), -1)
	for _, msg := range []*Message{real, marked, legacy, quoted, answer} {
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	_, items, err := store.GetTranscriptIndex(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetTranscriptIndex: %v", err)
	}
	if len(items) != 3 || items[0].ID != real.ID || items[1].ID != quoted.ID || items[2].ID != answer.ID {
		t.Fatalf("visible transcript items = %#v", items)
	}
	_, bodies, err := store.GetMessagesByTranscriptRanges(ctx, sess.ID, []TranscriptRange{{
		StartSeq: real.Sequence, StartID: real.ID, EndSeq: answer.Sequence, EndID: answer.ID,
	}})
	if err != nil {
		t.Fatalf("GetMessagesByTranscriptRanges: %v", err)
	}
	if len(bodies) != 3 || bodies[0].ID != real.ID || bodies[1].ID != quoted.ID || bodies[2].ID != answer.ID {
		t.Fatalf("visible transcript bodies = %#v", bodies)
	}
	loaded, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(loaded) != 5 || !loaded[1].IsGoalSteering() || loaded[1].TextContent != "" || !loaded[2].IsGoalSteering() || loaded[3].IsGoalSteering() {
		t.Fatalf("persisted goal steering classification = %#v", loaded)
	}
	providerMessage := loaded[1].ToLLMMessage()
	if llm.HasGoalSteeringPart(providerMessage.Parts) || llm.MessageText(providerMessage) != "marked internal continuation" {
		t.Fatalf("provider replay message = %#v", providerMessage)
	}
	if results, searchErr := store.Search(ctx, SearchOptions{Query: "marked internal continuation", Limit: 10}); searchErr != nil || len(results) != 0 {
		t.Fatalf("goal steering entered transcript search: results=%#v err=%v", results, searchErr)
	}
}

func TestSQLiteResponseRunStartStateMatchesTranscriptSnapshot(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	compacted := []Message{
		*NewMessage(sess.ID, llm.UserText("[Context Compaction]\nsummary"), -1),
		*NewMessage(sess.ID, llm.AssistantText("retained answer"), -1),
	}
	compacted[1].CompactionTail = true
	if err := store.CompactMessages(ctx, sess.ID, compacted); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	for _, message := range []*Message{
		NewMessage(sess.ID, llm.Message{Role: llm.RoleEvent, Parts: []llm.Part{{Type: llm.PartText, Text: "visible event"}}}, -1),
		NewMessage(sess.ID, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "provider context"}}}, -1),
	} {
		if err := store.AddMessage(ctx, sess.ID, message); err != nil {
			t.Fatalf("AddMessage(%s): %v", message.Role, err)
		}
	}

	assertMatchesSnapshot := func() ResponseRunStartState {
		t.Helper()
		snapshot, err := store.GetTranscriptSnapshot(ctx, sess.ID)
		if err != nil {
			t.Fatalf("GetTranscriptSnapshot: %v", err)
		}
		var wantBoundary int64
		for i := len(snapshot.Items) - 1; i >= 0; i-- {
			item := snapshot.Items[i]
			if item.Flags&TranscriptFlagCompactionTail != 0 {
				continue
			}
			switch llm.Role(item.Role) {
			case llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
				wantBoundary = item.ID
			}
			if wantBoundary != 0 {
				break
			}
		}
		state, err := store.GetResponseRunStartState(ctx, sess.ID)
		if err != nil {
			t.Fatalf("GetResponseRunStartState: %v", err)
		}
		if state.Rev != snapshot.Rev ||
			state.CompactionSeq != snapshot.CompactionSeq ||
			state.CompactionCount != snapshot.CompactionCount ||
			state.DurableBoundaryID != wantBoundary {
			t.Fatalf("start state = %#v, snapshot = %#v, want boundary %d", state, snapshot, wantBoundary)
		}
		return state
	}

	compactedState := assertMatchesSnapshot()
	if compactedState.CompactionSeq < 0 || compactedState.CompactionCount != 1 {
		t.Fatalf("compaction state = %#v, want active first compaction", compactedState)
	}
	tool := NewMessage(sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{
		Type:       llm.PartToolResult,
		ToolResult: &llm.ToolResult{ID: "call-latest", Name: "shell", Content: "done"},
	}}}, -1)
	if err := store.AddMessage(ctx, sess.ID, tool); err != nil {
		t.Fatalf("AddMessage(tool): %v", err)
	}
	if state := assertMatchesSnapshot(); state.DurableBoundaryID != tool.ID {
		t.Fatalf("boundary = %d, want latest tool row %d", state.DurableBoundaryID, tool.ID)
	}
}

func TestSQLiteResponseRunStartStateDoesNotQueueBehindWriter(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin writer transaction: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := store.GetResponseRunStartState(ctx, sess.ID); err != nil {
		t.Fatalf("start-state read queued behind occupied writer connection: %v", err)
	}
	cancel()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback writer transaction: %v", err)
	}

	readTx, err := store.readDB.Begin()
	if err != nil {
		t.Fatalf("begin transcript read transaction: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	if _, err := store.GetResponseRunStartState(ctx, sess.ID); err != nil {
		t.Fatalf("start-state read queued behind occupied transcript reader: %v", err)
	}
	cancel()
	if err := readTx.Rollback(); err != nil {
		t.Fatalf("rollback transcript read transaction: %v", err)
	}
}

func TestSQLiteResponseRunStartStateCompatibilityAndErrors(t *testing.T) {
	t.Run("unknown session", func(t *testing.T) {
		store, _ := newTranscriptTestStore(t)
		if _, err := store.GetResponseRunStartState(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("unsupported transcript revision", func(t *testing.T) {
		store, sess := newTranscriptTestStore(t)
		store.hasTranscriptRev = false
		if _, err := store.GetResponseRunStartState(context.Background(), sess.ID); !errors.Is(err, ErrTranscriptRevisionUnsupported) {
			t.Fatalf("error = %v, want ErrTranscriptRevisionUnsupported", err)
		}
	})

	t.Run("unsupported messages table", func(t *testing.T) {
		store, sess := newTranscriptTestStore(t)
		store.hasMessagesTable = false
		if _, err := store.GetResponseRunStartState(context.Background(), sess.ID); !errors.Is(err, ErrTranscriptRevisionUnsupported) {
			t.Fatalf("error = %v, want ErrTranscriptRevisionUnsupported", err)
		}
	})

	t.Run("legacy optional columns", func(t *testing.T) {
		store, sess := newTranscriptTestStore(t)
		store.hasCompactionSeq = false
		store.hasCompactionCount = false
		store.hasMessageCompactionTail = false
		state, err := store.GetResponseRunStartState(context.Background(), sess.ID)
		if err != nil {
			t.Fatalf("GetResponseRunStartState: %v", err)
		}
		if state.CompactionSeq != -1 || state.CompactionCount != 0 {
			t.Fatalf("legacy compaction state = %#v, want sequence -1 count 0", state)
		}
	})
}

func TestSQLiteCompactionPreservesResponseIdentity(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	messages := []Message{
		*NewMessage(sess.ID, llm.UserText("summary"), -1),
		*NewMessage(sess.ID, llm.AssistantText("retained answer"), -1),
	}
	messages[1].ResponseID = "resp-compaction-identity"
	messages[1].AssistantSegmentOrdinal = 0
	if err := store.CompactMessages(ctx, sess.ID, messages); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetTranscriptSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range snapshot.Items {
		if item.ResponseID == "resp-compaction-identity" && item.AssistantSegmentOrdinal == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("response identity did not survive compaction: %#v", snapshot.Items)
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

func TestSQLiteStoreTranscriptIndexIncludesAskUserResultBody(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	msg := NewMessage(sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{
		Type:       llm.PartToolResult,
		ToolResult: &llm.ToolResult{ID: "call-ask", Name: "ask_user", Content: `{"answers":[{"header":"Choice","selected":"A"}]}`},
	}}}, -1)
	if err := store.AddMessage(context.Background(), sess.ID, msg); err != nil {
		t.Fatal(err)
	}
	_, items, err := store.GetTranscriptIndex(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Flags&TranscriptFlagEmptyBody != 0 {
		t.Fatalf("ask_user result index = %#v, want materialized body", items)
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

func TestSQLiteStoreTranscriptPersistsResponseScopedAssistantIdentity(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	assistant := llm.AssistantText("stable segment")
	assistant.ResponseID = "resp_identity"
	assistant.AssistantSegmentOrdinal = 2
	assistant.SegmentStartSequence = 11
	assistant.SegmentEndSequence = 17
	message := NewMessage(sess.ID, assistant, -1)
	if err := store.AddMessage(ctx, sess.ID, message); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	snapshot, err := store.GetTranscriptSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetTranscriptSnapshot: %v", err)
	}
	if len(snapshot.Items) != 1 {
		t.Fatalf("items=%d want 1", len(snapshot.Items))
	}
	item := snapshot.Items[0]
	if item.ResponseID != assistant.ResponseID || item.AssistantSegmentOrdinal != 2 {
		t.Fatalf("index identity=%+v", item)
	}
	_, bodies, err := store.GetMessagesByTranscriptRanges(ctx, sess.ID, []TranscriptRange{{
		StartSeq: item.Seq, StartID: item.ID, EndSeq: item.Seq, EndID: item.ID,
	}})
	if err != nil {
		t.Fatalf("GetMessagesByTranscriptRanges: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("bodies=%d want 1", len(bodies))
	}
	got := bodies[0]
	if got.ResponseID != assistant.ResponseID || got.AssistantSegmentOrdinal != 2 || got.SegmentStartSequence != 11 || got.SegmentEndSequence != 17 {
		t.Fatalf("body identity=%+v", got)
	}
	roundTrip := got.ToLLMMessage()
	if roundTrip.ResponseID != assistant.ResponseID || roundTrip.AssistantSegmentOrdinal != 2 {
		t.Fatalf("LLM round trip identity=%+v", roundTrip)
	}
}
