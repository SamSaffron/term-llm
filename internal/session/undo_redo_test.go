package session

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	planpkg "github.com/samsaffron/term-llm/internal/plan"
)

func newUndoRedoSQLiteStore(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return store, path
}

func addUndoRedoMessage(t *testing.T, store *SQLiteStore, sessionID string, msg llm.Message, at time.Time) Message {
	t.Helper()
	stored := NewMessage(sessionID, msg, -1)
	stored.CreatedAt = at
	if err := store.AddMessage(context.Background(), sessionID, stored); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	return *stored
}

func TestSQLiteUndoRedoShrinksAndExactlyRestoresTranscriptAcrossRestart(t *testing.T) {
	store, path := newUndoRedoSQLiteStore(t)
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat, TitleSource: TitleSourceGenerated,
		GeneratedShortTitle: "Generated", GeneratedLongTitle: "Generated conversation", TitleBasisMsgSeq: 3,
		TitleGeneratedAt: time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	base := time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC)
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("first"), base)
	if err := store.IncrementUserTurns(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	addUndoRedoMessage(t, store, sess.ID, llm.AssistantText("first answer"), base.Add(time.Second))
	latest := llm.UserText("second exact prompt")
	latest.ClientMessageID = "client_second"
	addUndoRedoMessage(t, store, sess.ID, latest, base.Add(2*time.Second))
	if err := store.IncrementUserTurns(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	assistant := llm.AssistantText("second answer")
	assistant.ResponseID = "resp_second"
	assistant.AssistantSegmentOrdinal = 0
	addUndoRedoMessage(t, store, sess.ID, assistant, base.Add(3*time.Second))
	addUndoRedoMessage(t, store, sess.ID, llm.Message{Role: llm.RoleEvent, Parts: []llm.Part{{Type: llm.PartText, Text: "tail event"}}}, base.Add(4*time.Second))

	before, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages before: %v", err)
	}
	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatalf("TranscriptMutationState: %v", err)
	}
	result, err := store.UndoLastUserTurn(ctx, sess.ID, state)
	if err != nil {
		t.Fatalf("UndoLastUserTurn: %v", err)
	}
	if result.UserText != "second exact prompt" {
		t.Fatalf("undo user text = %q", result.UserText)
	}
	afterUndo, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after undo: %v", err)
	}
	if len(afterUndo) != 2 {
		t.Fatalf("transcript length after undo = %d, want 2", len(afterUndo))
	}
	if result.Rev != state.Rev+1 || result.HeadID != afterUndo[len(afterUndo)-1].ID {
		t.Fatalf("undo state = %+v, before = %+v", result.TranscriptMutationState, state)
	}
	refreshed, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get session after undo: %v", err)
	}
	if refreshed.UserTurns != 2 || refreshed.Summary != "first" || refreshed.LastTotalTokens != 0 || refreshed.TitleSource == TitleSourceGenerated {
		t.Fatalf("derived metadata was not reconciled without decreasing user turns: %+v", refreshed)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store, err = NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	restartState, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatalf("state after restart: %v", err)
	}
	redo, err := store.RedoLastUserTurn(ctx, sess.ID, restartState)
	if err != nil {
		t.Fatalf("RedoLastUserTurn after restart: %v", err)
	}
	afterRedo, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after redo: %v", err)
	}
	if !reflect.DeepEqual(afterRedo, before) {
		t.Fatalf("redo transcript differs\n got: %#v\nwant: %#v", afterRedo, before)
	}
	if redo.Rev != restartState.Rev+1 || redo.HeadID != before[len(before)-1].ID {
		t.Fatalf("redo state = %+v, undo state = %+v", redo.TranscriptMutationState, restartState)
	}
	refreshed, err = store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get session after redo: %v", err)
	}
	if refreshed.UserTurns != 2 || refreshed.Summary != "first" || refreshed.GeneratedShortTitle != "Generated" || refreshed.TitleSource != TitleSourceGenerated {
		t.Fatalf("redo metadata was not restored: %+v", refreshed)
	}
}

func TestSQLiteUndoRedoOptimisticChecksAndMutationInvalidation(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("one"), time.Now())
	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UndoLastUserTurn(ctx, sess.ID, TranscriptMutationState{Rev: state.Rev - 1, HeadID: state.HeadID}); !errors.Is(err, ErrTranscriptConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	if _, err := store.UndoLastUserTurn(ctx, sess.ID, TranscriptMutationState{Rev: state.Rev, HeadID: state.HeadID + 1}); !errors.Is(err, ErrTranscriptConflict) {
		t.Fatalf("stale head error = %v", err)
	}
	undo, err := store.UndoLastUserTurn(ctx, sess.ID, state)
	if err != nil {
		t.Fatal(err)
	}
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("replacement"), time.Now().Add(time.Second))
	current, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedoLastUserTurn(ctx, sess.ID, undo.TranscriptMutationState); !errors.Is(err, ErrTranscriptConflict) {
		t.Fatalf("stale redo error = %v", err)
	}
	if _, err := store.RedoLastUserTurn(ctx, sess.ID, current); !errors.Is(err, ErrNothingToRedo) {
		t.Fatalf("invalidated redo error = %v", err)
	}
}

func TestSQLiteOrdinaryTranscriptMutationsInvalidateRedo(t *testing.T) {
	mutations := map[string]func(*testing.T, context.Context, *SQLiteStore, string){
		"add": func(t *testing.T, ctx context.Context, store *SQLiteStore, sessionID string) {
			addUndoRedoMessage(t, store, sessionID, llm.UserText("new turn"), time.Now())
		},
		"update": func(t *testing.T, ctx context.Context, store *SQLiteStore, sessionID string) {
			messages, err := store.GetMessages(ctx, sessionID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			messages[len(messages)-1].Parts = []llm.Part{{Type: llm.PartText, Text: "edited answer"}}
			messages[len(messages)-1].TextContent = "edited answer"
			if err := store.UpdateMessage(ctx, sessionID, &messages[len(messages)-1]); err != nil {
				t.Fatal(err)
			}
		},
		"replace": func(t *testing.T, ctx context.Context, store *SQLiteStore, sessionID string) {
			messages, err := store.GetMessages(ctx, sessionID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ReplaceMessages(ctx, sessionID, messages); err != nil {
				t.Fatal(err)
			}
		},
		"compact": func(t *testing.T, ctx context.Context, store *SQLiteStore, sessionID string) {
			summary := NewMessage(sessionID, llm.UserText("[Context Compaction]\nsummary"), 0)
			if err := store.CompactMessages(ctx, sessionID, []Message{*summary}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			store, _ := newUndoRedoSQLiteStore(t)
			defer store.Close()
			ctx := context.Background()
			sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
			if err := store.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			addUndoRedoMessage(t, store, sess.ID, llm.UserText("first"), time.Now())
			addUndoRedoMessage(t, store, sess.ID, llm.AssistantText("answer"), time.Now().Add(time.Millisecond))
			addUndoRedoMessage(t, store, sess.ID, llm.UserText("undo me"), time.Now().Add(2*time.Millisecond))
			state, err := store.TranscriptMutationState(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.UndoLastUserTurn(ctx, sess.ID, state); err != nil {
				t.Fatal(err)
			}
			mutate(t, ctx, store, sess.ID)
			current, err := store.TranscriptMutationState(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.RedoLastUserTurn(ctx, sess.ID, current); !errors.Is(err, ErrNothingToRedo) {
				t.Fatalf("redo after %s mutation error = %v", name, err)
			}
		})
	}
}

func TestSQLiteUndoPreservesCompactionBoundaryAndSkipsSyntheticUsers(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	base := time.Now()
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("old real prompt"), base)
	addUndoRedoMessage(t, store, sess.ID, llm.AssistantText("old answer"), base.Add(time.Second))
	summary := NewMessage(sess.ID, llm.UserText("[Context Compaction]\nsummary"), 0)
	ack := NewMessage(sess.ID, llm.AssistantText("context loaded"), 1)
	tail := NewMessage(sess.ID, llm.UserText("retained real-looking tail"), 2)
	tail.CompactionTail = true
	if err := store.CompactMessages(ctx, sess.ID, []Message{*summary, *ack, *tail}); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	compacted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UndoLastUserTurn(ctx, sess.ID, state); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("synthetic-only undo error = %v", err)
	}
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("post compact prompt"), base.Add(2*time.Second))
	addUndoRedoMessage(t, store, sess.ID, llm.AssistantText("post compact answer"), base.Add(3*time.Second))
	state, _ = store.TranscriptMutationState(ctx, sess.ID)
	if _, err := store.UndoLastUserTurn(ctx, sess.ID, state); err != nil {
		t.Fatalf("UndoLastUserTurn post compact: %v", err)
	}
	refreshed, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.CompactionSeq != compacted.CompactionSeq || refreshed.CompactionCount != compacted.CompactionCount {
		t.Fatalf("compaction boundary changed from (%d,%d) to (%d,%d)", compacted.CompactionSeq, compacted.CompactionCount, refreshed.CompactionSeq, refreshed.CompactionCount)
	}
}

func TestSQLiteUndoReconcilesPlanAndClearsProviderState(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", ProviderKey: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	firstPlan := `{"plan":[{"step":"first","status":"completed"}]}`
	secondPlan := `{"plan":[{"step":"second","status":"in_progress"}]}`
	addPlan := func(prompt, callID, args string, at time.Time) {
		addUndoRedoMessage(t, store, sess.ID, llm.UserText(prompt), at)
		addUndoRedoMessage(t, store, sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: callID, Name: planpkg.ToolName, Arguments: []byte(args)}}}}, at.Add(time.Millisecond))
		addUndoRedoMessage(t, store, sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: callID, Name: planpkg.ToolName, Content: "ok"}}}}, at.Add(2*time.Millisecond))
	}
	addPlan("first prompt", "plan_1", firstPlan, time.Now())
	addPlan("second prompt", "plan_2", secondPlan, time.Now().Add(time.Second))
	secondSnapshot, err := planpkg.Parse([]byte(secondPlan))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePlanSnapshot(ctx, sess.ID, secondSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProviderState(ctx, sess.ID, "test", []byte(`{"continuation":"opaque"}`)); err != nil {
		t.Fatal(err)
	}
	state, _ := store.TranscriptMutationState(ctx, sess.ID)
	undo, err := store.UndoLastUserTurn(ctx, sess.ID, state)
	if err != nil {
		t.Fatal(err)
	}
	plan, version, err := store.LoadPlanSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if version == 0 || len(plan.Plan) != 1 || plan.Plan[0].Step != "first" {
		t.Fatalf("plan after undo = %#v version=%d", plan, version)
	}
	providerState, err := store.LoadProviderState(ctx, sess.ID, "test")
	if err != nil || len(providerState) != 0 {
		t.Fatalf("provider state after undo = %q err=%v", providerState, err)
	}
	if _, err := store.RedoLastUserTurn(ctx, sess.ID, undo.TranscriptMutationState); err != nil {
		t.Fatal(err)
	}
	plan, version, err = store.LoadPlanSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if version == 0 || len(plan.Plan) != 1 || plan.Plan[0].Step != "second" {
		t.Fatalf("plan after redo = %#v version=%d", plan, version)
	}
}

func TestSQLiteUndoCanonicalizesLongMultilineSummary(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	firstPrompt := strings.Repeat("long summary source ", 12) + "\nsecond line must never enter summary"
	wantSummary := TruncateSummary(firstPrompt)
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat, Summary: wantSummary}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	addUndoRedoMessage(t, store, sess.ID, llm.UserText(firstPrompt), base)
	if err := store.IncrementUserTurns(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	addUndoRedoMessage(t, store, sess.ID, llm.AssistantText("answer"), base.Add(time.Second))
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("undo me"), base.Add(2*time.Second))
	if err := store.IncrementUserTurns(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UndoLastUserTurn(ctx, sess.ID, state); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Summary != wantSummary || strings.Contains(refreshed.Summary, "\n") || len(refreshed.Summary) > 100 {
		t.Fatalf("summary = %q, want canonical %q", refreshed.Summary, wantSummary)
	}
}

func TestSQLiteUndoSkipsWhitespacePrefixedCompactionMarker(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	summary := NewMessage(sess.ID, llm.UserText("\n\t \r[Context Compaction]\nsummary"), 0)
	if err := store.CompactMessages(ctx, sess.ID, []Message{*summary}); err != nil {
		t.Fatal(err)
	}
	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UndoLastUserTurn(ctx, sess.ID, state); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("undo whitespace-prefixed compaction marker error = %v", err)
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("compaction marker was destructively selected: len=%d err=%v", len(messages), err)
	}
}

func TestSQLiteUndoPreservesEveryUserRowActivityAndMonotonicTurns(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("old real prompt"), base)
	if err := store.IncrementUserTurns(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	summary := NewMessage(sess.ID, llm.UserText("[Context Compaction]\nsummary"), -1)
	summary.CreatedAt = base.Add(10 * time.Second)
	tail := NewMessage(sess.ID, llm.UserText("retained tail"), -1)
	tail.CreatedAt = base.Add(11 * time.Second)
	tail.CompactionTail = true
	if err := store.CompactMessages(ctx, sess.ID, []Message{*summary, *tail}); err != nil {
		t.Fatal(err)
	}
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("post-compaction prompt"), base.Add(20*time.Second))
	if err := store.IncrementUserTurns(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UndoLastUserTurn(ctx, sess.ID, state); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.UserTurns != 2 {
		t.Fatalf("UserTurns = %d, want increment-only value 2", refreshed.UserTurns)
	}
	var lastUserMessageAt time.Time
	if err := store.db.QueryRowContext(ctx, `SELECT last_user_message_at FROM sessions WHERE id = ?`, sess.ID).Scan(&lastUserMessageAt); err != nil {
		t.Fatal(err)
	}
	if !lastUserMessageAt.Equal(tail.CreatedAt) {
		t.Fatalf("last_user_message_at = %v, want retained user row time %v", lastUserMessageAt, tail.CreatedAt)
	}
}

func TestSQLiteUndoConcurrentRequestsAllowOneWinner(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("race"), time.Now())
	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := store.UndoLastUserTurn(ctx, sess.ID, state)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	var succeeded, conflicted int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrTranscriptConflict):
			conflicted++
		default:
			t.Fatalf("concurrent undo error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent undo results: succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func TestSQLiteRedoRowCascadesWhenSessionDeleted(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	addUndoRedoMessage(t, store, sess.ID, llm.UserText("delete me"), time.Now())
	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UndoLastUserTurn(ctx, sess.ID, state); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_redo WHERE session_id = ?`, sess.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("redo row before delete count=%d err=%v", count, err)
	}
	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_redo WHERE session_id = ?`, sess.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("redo row after cascade count=%d err=%v", count, err)
	}
}

func TestSQLiteUndoReportsAttachmentsOmittedFromComposer(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	message := llm.UserText("describe this")
	message.Parts = append(message.Parts, llm.Part{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}})
	addUndoRedoMessage(t, store, sess.ID, message, time.Now())
	state, err := store.TranscriptMutationState(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.UndoLastUserTurn(ctx, sess.ID, state)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AttachmentsOmitted || result.UserText != "describe this" {
		t.Fatalf("undo result = %+v, want text with attachment warning", result)
	}
}
