package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSteeringFIFOIdentityAndRushTransaction(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	entries := []PendingSteering{}
	for _, id := range []string{"z", "a"} {
		msg := llm.UserText("guidance " + id)
		msg.ClientMessageID = id
		entry := PendingSteering{SessionID: sess.ID, ID: id, Message: msg, DisplayText: llm.MessageText(msg), CreatedAt: time.Unix(1, 0), Origin: llm.SteeringOriginUser}
		if err := store.SavePendingSteering(ctx, entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	pending, err := store.ListPendingSteering(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != "z" || pending[1].ID != "a" {
		t.Fatalf("FIFO lost: %+v", pending)
	}
	conflict := entries[0]
	conflict.DisplayText = "changed"
	if err := store.SavePendingSteering(ctx, conflict); !errors.Is(err, ErrSteeringConflict) {
		t.Fatalf("conflict: %v", err)
	}
	op, err := store.AdmitRush(ctx, RushOperation{SessionID: sess.ID, RequestID: "rush", SourceResponseID: "source", SourceEpoch: 1, Fence: 1, ReplacementResponseID: "replacement"}, entries)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.AdmitRush(ctx, RushOperation{SessionID: sess.ID, RequestID: "rush", SourceResponseID: "source", SourceEpoch: 1, Fence: 1}, nil)
	if err != nil || len(replay.Entries) != 2 {
		t.Fatalf("retry changed batch: %+v %v", replay, err)
	}
	if err := store.DeletePendingSteering(ctx, sess.ID, "z"); !errors.Is(err, ErrSteeringConflict) {
		t.Fatal(err)
	}
	pending, err = store.ListPendingSteering(ctx, sess.ID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("delete stole rush entry: %+v %v", pending, err)
	}
	op, err = store.AdvanceRush(ctx, op, RushStarting, "")
	if err != nil {
		t.Fatal(err)
	}
	rows := []*Message{NewMessage(sess.ID, entries[0].Message, -1), NewMessage(sess.ID, entries[1].Message, -1)}
	if _, err := store.CommitRushInitialInput(ctx, op, rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].ID == 0 || rows[1].Sequence != rows[0].Sequence+1 {
		t.Fatalf("nonconsecutive rows: %+v", rows)
	}
	if _, err := store.CommitRushInitialInput(ctx, op, rows); !errors.Is(err, ErrSteeringConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	committed, err := store.GetRush(ctx, sess.ID, "rush")
	if err != nil || committed.Status != RushStarted || committed.Entries[0].Disposition != "committed" {
		t.Fatalf("commit metadata: %+v %v", committed, err)
	}
	pending, err = store.ListPendingSteering(ctx, sess.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after commit: %+v %v", pending, err)
	}
}

func TestRushStopCASPreventsInitialInput(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	ctx := context.Background()
	msg := llm.UserText("keep me")
	msg.ClientMessageID = "one"
	entry := PendingSteering{SessionID: sess.ID, ID: "one", Message: msg, DisplayText: "keep me", Origin: llm.SteeringOriginUser}
	if err := store.SavePendingSteering(ctx, entry); err != nil {
		t.Fatal(err)
	}
	op, err := store.AdmitRush(ctx, RushOperation{SessionID: sess.ID, RequestID: "rush", SourceResponseID: "source", SourceEpoch: 1, Fence: 1, ReplacementResponseID: "replacement"}, []PendingSteering{entry})
	if err != nil {
		t.Fatal(err)
	}
	op, err = store.AdvanceRush(ctx, op, RushStarting, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceRush(ctx, op, RushCancelled, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRushInitialInput(ctx, op, []*Message{NewMessage(sess.ID, msg, -1)}); !errors.Is(err, ErrSteeringConflict) {
		t.Fatalf("stopped input committed: %v", err)
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil || len(messages) != 0 {
		t.Fatalf("leaked rows: %+v %v", messages, err)
	}
	op, err = store.GetRush(ctx, sess.ID, "rush")
	if err != nil || len(op.Entries) != 1 {
		t.Fatalf("lost stopped guidance: %+v %v", op, err)
	}
}
