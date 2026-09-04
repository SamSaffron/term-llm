package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func newAttentionTestStore(t *testing.T) (*SQLiteStore, context.Context, string) {
	t.Helper()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	id := NewID()
	if err := store.Create(ctx, &Session{ID: id, Provider: "test", Model: "test", Origin: OriginWeb, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return store, ctx, id
}

func admitAttentionRun(t *testing.T, store *SQLiteStore, ctx context.Context, sessionID, responseID, owner string, startedRev int64) ResponseRunLease {
	t.Helper()
	lease, err := store.AdmitResponseRun(ctx, ResponseRunAdmission{ResponseID: responseID, SessionID: sessionID,
		RunEpoch: time.Now().UnixNano(), OwnerInstanceID: owner, StartedRev: startedRev, StartedAt: time.Now(), LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestAttentionSequencePreventsLateAcknowledgementFromClearingNewRun(t *testing.T) {
	store, ctx, sessionID := newAttentionTestStore(t)
	storeID, err := store.StoreInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstLease := admitAttentionRun(t, store, ctx, sessionID, "resp_first", "owner", 1)
	first, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "resp_first", OwnerInstanceID: "owner",
		FencingToken: firstLease.FencingToken, Outcome: ResponseRunCompleted, FinalRev: 2, DurableOutputCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	secondLease := admitAttentionRun(t, store, ctx, sessionID, "resp_second", "owner", 2)
	second, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "resp_second", OwnerInstanceID: "owner",
		FencingToken: secondLease.FencingToken, Outcome: ResponseRunFailed, FinalRev: 3})
	if err != nil {
		t.Fatal(err)
	}
	if second.LatestAttentionSeq <= first.LatestAttentionSeq {
		t.Fatalf("markers did not advance: first=%d second=%d", first.LatestAttentionSeq, second.LatestAttentionSeq)
	}
	state, err := store.MarkAttentionSeen(ctx, sessionID, storeID, first.LatestAttentionSeq)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Unseen || state.SeenThroughSeq != first.LatestAttentionSeq || state.LatestAttentionSeq != second.LatestAttentionSeq {
		t.Fatalf("late acknowledgement cleared newer marker: %+v", state)
	}
	if _, err := store.MarkAttentionSeen(ctx, sessionID, storeID, second.LatestAttentionSeq+1); !errors.Is(err, ErrAttentionConflict) {
		t.Fatalf("over-latest acknowledgement error = %v", err)
	}
	state, err = store.MarkAttentionSeen(ctx, sessionID, storeID, second.LatestAttentionSeq)
	if err != nil || state.Unseen {
		t.Fatalf("latest acknowledgement = %+v, %v", state, err)
	}
}

func TestResponseRunFenceIsAtomicWithTranscriptWriteAndCheckpoint(t *testing.T) {
	store, ctx, sessionID := newAttentionTestStore(t)
	lease := admitAttentionRun(t, store, ctx, sessionID, "resp_fenced", "owner", 0)
	fencedCtx := WithResponseRunFence(ctx, ResponseRunFence{ResponseID: "resp_fenced", OwnerInstanceID: "owner",
		FencingToken: lease.FencingToken, DurableOutputCount: 1})
	first := &Message{Role: llm.RoleAssistant, TextContent: "durable", Sequence: -1, ResponseID: "resp_fenced"}
	rev, err := store.AddMessageWithTranscriptRev(fencedCtx, sessionID, first)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "resp_fenced", OwnerInstanceID: "owner",
		FencingToken: lease.FencingToken, Outcome: ResponseRunCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if state.FinalRev != rev {
		t.Fatalf("atomic checkpoint final_rev=%d, want %d", state.FinalRev, rev)
	}
	stale := &Message{Role: llm.RoleAssistant, TextContent: "late", Sequence: -1, ResponseID: "resp_fenced"}
	if _, err := store.AddMessageWithTranscriptRev(fencedCtx, sessionID, stale); !errors.Is(err, ErrResponseRunLeaseLost) {
		t.Fatalf("stale fenced write error = %v", err)
	}
	batch := []*Message{
		{Role: llm.RoleDeveloper, TextContent: "activity", Sequence: -1, ResponseID: "resp_fenced"},
		{Role: llm.RoleUser, TextContent: "retry", Sequence: -1, ResponseID: "resp_fenced"},
	}
	if _, err := store.AppendMessagesWithTranscriptRev(fencedCtx, sessionID, batch); !errors.Is(err, ErrResponseRunLeaseLost) {
		t.Fatalf("stale fenced batch error = %v", err)
	}
	if batch[0].ID != 0 || batch[1].ID != 0 {
		t.Fatalf("rolled-back batch mutated IDs: %#v", batch)
	}
	messages, err := store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].TextContent != "durable" {
		t.Fatalf("stale transcript write committed: %+v", messages)
	}
}

func TestSharedStoreRecoveryFencesOtherProcessTranscriptWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	ownerStore, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer ownerStore.Close()
	recoveryStore, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer recoveryStore.Close()
	ctx := context.Background()
	sessionID := NewID()
	if err := ownerStore.Create(ctx, &Session{ID: sessionID, Provider: "test", Model: "test", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	lease := admitAttentionRun(t, ownerStore, ctx, sessionID, "resp_shared", "process-one", 0)
	if _, err := ownerStore.db.ExecContext(ctx, `UPDATE serve_response_lifecycle SET lease_expires_at=? WHERE response_id=?`, time.Now().Add(-time.Minute).UnixMilli(), "resp_shared"); err != nil {
		t.Fatal(err)
	}
	if recovered, err := recoveryStore.RecoverExpiredResponseRuns(ctx, 10); err != nil || len(recovered) != 1 {
		t.Fatalf("shared-store recovery = %+v, %v", recovered, err)
	}
	fencedCtx := WithResponseRunFence(ctx, ResponseRunFence{ResponseID: "resp_shared", OwnerInstanceID: "process-one",
		FencingToken: lease.FencingToken, DurableOutputCount: 1})
	late := &Message{Role: llm.RoleAssistant, TextContent: "late output", Sequence: -1, ResponseID: "resp_shared"}
	if _, err := ownerStore.AddMessageWithTranscriptRev(fencedCtx, sessionID, late); !errors.Is(err, ErrResponseRunLeaseLost) {
		t.Fatalf("other-process stale write error = %v", err)
	}
	messages, err := recoveryStore.GetMessages(ctx, sessionID, 0, 0)
	if err != nil || len(messages) != 0 {
		t.Fatalf("other-process stale output committed: %+v, %v", messages, err)
	}
}

func TestGetAttentionBatchChunksBeyondSQLiteVariableLimit(t *testing.T) {
	store, ctx, firstID := newAttentionTestStore(t)
	lastID := NewID()
	if err := store.Create(ctx, &Session{ID: lastID, Provider: "test", Model: "test", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{firstID, lastID} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO session_attention(session_id,latest_attention_seq,response_id,outcome,updated_at)
			VALUES(?,?,?,?,?)`, id, index+1, fmt.Sprintf("resp-%d", index), ResponseRunCompleted, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	ids := make([]string, 0, 501)
	ids = append(ids, firstID)
	for index := 1; index < 500; index++ {
		ids = append(ids, fmt.Sprintf("missing-%03d", index))
	}
	ids = append(ids, lastID)
	states, err := store.GetAttentionBatch(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[lastID].LatestAttentionSeq != 2 {
		t.Fatalf("chunked attention states = %+v", states)
	}
}

func TestCancelledAttentionRequiresDurableOutput(t *testing.T) {
	for _, test := range []struct {
		name     string
		finalRev int64
		outputs  int
		want     bool
	}{
		{name: "empty", finalRev: 7, want: false},
		{name: "revision advanced", finalRev: 8, want: true},
		{name: "durable output", finalRev: 7, outputs: 1, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, ctx, sessionID := newAttentionTestStore(t)
			lease := admitAttentionRun(t, store, ctx, sessionID, "resp_cancel", "owner", 7)
			state, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "resp_cancel", OwnerInstanceID: "owner",
				FencingToken: lease.FencingToken, Outcome: ResponseRunCancelled, FinalRev: test.finalRev, DurableOutputCount: test.outputs})
			if err != nil {
				t.Fatal(err)
			}
			if state.Unseen != test.want {
				t.Fatalf("unseen=%v, want %v: %+v", state.Unseen, test.want, state)
			}
		})
	}
}

func TestOwnerCanFinalizeAfterLeaseExpiryBeforeOrphanCAS(t *testing.T) {
	store, ctx, sessionID := newAttentionTestStore(t)
	lease := admitAttentionRun(t, store, ctx, sessionID, "resp_grace", "owner", 1)
	if _, err := store.db.ExecContext(ctx, `UPDATE serve_response_lifecycle SET lease_expires_at=? WHERE response_id=?`, time.Now().Add(-time.Second).UnixMilli(), "resp_grace"); err != nil {
		t.Fatal(err)
	}
	state, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "resp_grace", OwnerInstanceID: "owner",
		FencingToken: lease.FencingToken, Outcome: ResponseRunCompleted, FinalRev: 2})
	if err != nil || !state.Unseen || state.Outcome != ResponseRunCompleted {
		t.Fatalf("owned finalization inside recovery grace = %+v, %v", state, err)
	}
}

func TestExpiredRunRecoveryFencesLateOwner(t *testing.T) {
	store, ctx, sessionID := newAttentionTestStore(t)
	lease := admitAttentionRun(t, store, ctx, sessionID, "resp_orphan", "dead-owner", 4)
	if _, err := store.db.ExecContext(ctx, `UPDATE serve_response_lifecycle SET lease_expires_at=? WHERE response_id=?`, time.Now().Add(-time.Minute).UnixMilli(), "resp_orphan"); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverExpiredResponseRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Outcome != ResponseRunOrphaned || !recovered[0].Unseen {
		t.Fatalf("recovered = %+v", recovered)
	}
	if err := store.CheckpointResponseRun(ctx, ResponseRunCheckpoint{ResponseID: "resp_orphan", OwnerInstanceID: "dead-owner", FencingToken: lease.FencingToken, FinalRev: 99}); !errors.Is(err, ErrResponseRunLeaseLost) {
		t.Fatalf("late checkpoint error = %v", err)
	}
	if _, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "resp_orphan", OwnerInstanceID: "dead-owner", FencingToken: lease.FencingToken, Outcome: ResponseRunCompleted, FinalRev: 9}); !errors.Is(err, ErrResponseRunLeaseLost) {
		t.Fatalf("late terminal error = %v", err)
	}
	state, err := store.GetAttention(ctx, sessionID)
	if err != nil || state.Outcome != ResponseRunOrphaned {
		t.Fatalf("orphan marker overwritten: %+v, %v", state, err)
	}
}

func TestAttentionSnapshotIncludesCompleteSetsAndStablePagination(t *testing.T) {
	store, ctx, sessionID := newAttentionTestStore(t)
	lease := admitAttentionRun(t, store, ctx, sessionID, "resp_done", "owner", 0)
	if _, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "resp_done", OwnerInstanceID: "owner", FencingToken: lease.FencingToken, Outcome: ResponseRunCompleted, FinalRev: 1}); err != nil {
		t.Fatal(err)
	}
	other := NewID()
	if err := store.Create(ctx, &Session{ID: other, Provider: "test", Model: "test", Origin: OriginWeb, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = admitAttentionRun(t, store, ctx, other, "resp_running", "owner", 0)
	unseen, err := store.ListAttention(ctx, AttentionListOptions{Kind: AttentionKindUnseen, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if unseen.ProtocolVersion != 1 || len(unseen.Items) != 1 || unseen.Items[0].SessionID != sessionID || unseen.Items[0].SessionNumber <= 0 {
		t.Fatalf("unseen page = %+v", unseen)
	}
	running, err := store.ListAttention(ctx, AttentionListOptions{Kind: AttentionKindRunning, Limit: 1, SnapshotVersion: unseen.SnapshotVersion})
	if err != nil {
		t.Fatal(err)
	}
	if len(running.Items) != 1 || running.Items[0].SessionID != other || running.Items[0].SessionNumber <= 0 || running.StoreInstanceID != unseen.StoreInstanceID {
		t.Fatalf("running page = %+v", running)
	}
}

func TestResponseInteractionRequiredIsLevelTriggeredAndFenced(t *testing.T) {
	store, ctx, sessionID := newAttentionTestStore(t)
	lease := admitAttentionRun(t, store, ctx, sessionID, "resp_input", "owner", 0)
	since := time.Now().UTC().Add(-time.Minute)
	if err := store.SetResponseRunInteractionState(ctx, ResponseRunInteractionState{
		ResponseID: "resp_input", OwnerInstanceID: "owner", FencingToken: lease.FencingToken,
		Revision: 4, Count: 2, Kinds: []string{"approval.shell", "ask_user"}, RequiredSince: since,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListAttention(ctx, AttentionListOptions{Kind: AttentionKindInputRequired, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.ProtocolVersion != 2 || len(page.Items) != 1 {
		t.Fatalf("input-required page = %+v", page)
	}
	item := page.Items[0]
	if item.SessionID != sessionID || !item.InteractionRequired || item.PendingInteractionCount != 2 ||
		len(item.PendingInteractionKinds) != 2 || item.InteractionStateRev != 4 {
		t.Fatalf("input-required item = %+v", item)
	}
	// An older asynchronous projection cannot clear a newer prompt set.
	if err := store.SetResponseRunInteractionState(ctx, ResponseRunInteractionState{
		ResponseID: "resp_input", OwnerInstanceID: "owner", FencingToken: lease.FencingToken, Revision: 3,
	}); err != nil {
		t.Fatal(err)
	}
	page, err = store.ListAttention(ctx, AttentionListOptions{Kind: AttentionKindInputRequired, Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].PendingInteractionCount != 2 {
		t.Fatalf("stale projection changed level: %+v, %v", page, err)
	}
	if err := store.SetResponseRunInteractionState(ctx, ResponseRunInteractionState{
		ResponseID: "resp_input", OwnerInstanceID: "other", FencingToken: lease.FencingToken, Revision: 5,
	}); !errors.Is(err, ErrResponseRunLeaseLost) {
		t.Fatalf("stale owner error = %v", err)
	}
	if _, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "resp_input", OwnerInstanceID: "owner",
		FencingToken: lease.FencingToken, Outcome: ResponseRunCompleted}); err != nil {
		t.Fatal(err)
	}
	page, err = store.ListAttention(ctx, AttentionListOptions{Kind: AttentionKindInputRequired, Limit: 10})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("terminal run remained input-required: %+v, %v", page, err)
	}
}

func TestReadOnlyStoreDoesNotAdvertiseDurableAttention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	writable, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := NewSQLiteStore(Config{Enabled: true, Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if _, ok := AsAttentionStore(readOnly); ok {
		t.Fatal("read-only store advertised attention writes")
	}
	if _, ok := AsAttentionBatchStore(readOnly); ok {
		t.Fatal("read-only store advertised attention projection")
	}
	if _, ok := AsServeResponseLifecycleStore(readOnly); ok {
		t.Fatal("read-only store advertised response lifecycle writes")
	}
	if _, ok := AsResponseRunInteractionStore(readOnly); ok {
		t.Fatal("read-only store advertised response interaction writes")
	}
}
