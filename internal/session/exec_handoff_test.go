package session

import (
	"errors"
	"github.com/samsaffron/term-llm/internal/llm"
	"testing"
	"time"
)

func TestExecHandoffAdmission(t *testing.T) {
	for _, variant := range []string{"consume", "same boot", "wrong service", "advance", "cancel", "new run", "discard", "expired", "rush", "steering"} {
		t.Run(variant, func(t *testing.T) {
			store, ctx, sid := newAttentionTestStore(t)
			lease := admitAttentionRun(t, store, ctx, sid, "source", "boot-A", 0)
			h := ExecHandoff{ID: "restart", ServiceID: "service", SourceOwnerID: "boot-A", SessionID: sid, SourceResponseID: "source", SourceFence: lease.FencingToken, CheckpointRev: 0, Request: []byte(`{}`)}
			if err := store.PrepareExecHandoff(ctx, []ExecHandoff{h}); err != nil {
				t.Fatal(err)
			}
			// Preparing cannot cancel or fence the live invocation: exec may fail.
			if err := store.ValidateResponseRunLease(ctx, "source", "boot-A", lease.FencingToken); err != nil {
				t.Fatal(err)
			}
			a := ResponseRunAdmission{ResponseID: "replacement", SessionID: sid, OwnerInstanceID: "boot-B", ExecRestartID: h.ID, ExecServiceID: h.ServiceID}
			switch variant {
			case "same boot":
				a.OwnerInstanceID = "boot-A"
			case "wrong service":
				a.ExecServiceID = "other"
			case "advance":
				if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET transcript_rev=transcript_rev+1 WHERE id=?`, sid); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				if _, err := store.FinalizeResponseRun(ctx, ResponseRunTerminal{ResponseID: "source", OwnerInstanceID: "boot-A", FencingToken: lease.FencingToken, Outcome: ResponseRunCancelled}); err != nil {
					t.Fatal(err)
				}
			case "new run":
				admitAttentionRun(t, store, ctx, sid, "newer", "boot-A", 0)
			case "discard":
				if err := store.DiscardExecHandoff(ctx, h.ID, h.SourceOwnerID); err != nil {
					t.Fatal(err)
				}
			case "steering":
				if err := store.SavePendingSteering(ctx, PendingSteering{SessionID: sid, ID: "queued", Message: llm.UserText("wait"), CreatedAt: time.Now(), Origin: llm.SteeringOriginUser}); err != nil {
					t.Fatal(err)
				}
			case "rush":
				if _, err := store.db.ExecContext(ctx, `INSERT INTO session_rush_operations(session_id,request_id,source_response_id,source_epoch,status,owner_fence,created_at,updated_at) VALUES(?, 'rush', 'source', 1, 'starting', 1, 1, 1)`, sid); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := store.db.ExecContext(ctx, `UPDATE serve_exec_handoffs SET created_at=0`); err != nil {
					t.Fatal(err)
				}
			}
			_, err := store.AdmitResponseRun(ctx, a)
			if variant != "consume" {
				if !errors.Is(err, ErrExecHandoffConflict) {
					t.Fatalf("admission error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ValidateResponseRunLease(ctx, "source", "boot-A", lease.FencingToken); !errors.Is(err, ErrResponseRunLeaseLost) {
				t.Fatalf("source fence: %v", err)
			}
			next, err := store.ExecContinuation(ctx, "source", h.ServiceID)
			if err != nil || next != "replacement" {
				t.Fatalf("cancel continuation=%q err=%v", next, err)
			}
			other, err := store.ExecContinuation(ctx, "source", "other-service")
			if err != nil || other != "source" {
				t.Fatalf("cross-service continuation=%q err=%v", other, err)
			}
			a.ResponseID = "duplicate"
			if _, err := store.AdmitResponseRun(ctx, a); !errors.Is(err, ErrExecHandoffConflict) {
				t.Fatalf("duplicate=%v", err)
			}
			entries, err := store.ReadExecHandoff(ctx, h.ID, h.ServiceID)
			if err != nil || len(entries) != 0 {
				t.Fatalf("consumed entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestExecHandoffBatchPrepareRollsBack(t *testing.T) {
	store, ctx, sid := newAttentionTestStore(t)
	lease := admitAttentionRun(t, store, ctx, sid, "source", "boot-A", 0)
	h := ExecHandoff{ID: "restart", ServiceID: "service", SourceOwnerID: "boot-A", SessionID: sid, SourceResponseID: "source", SourceFence: lease.FencingToken, Request: []byte(`{}`)}
	bad := h
	bad.SessionID = "missing"
	if err := store.PrepareExecHandoff(ctx, []ExecHandoff{h, bad}); !errors.Is(err, ErrExecHandoffConflict) {
		t.Fatalf("prepare: %v", err)
	}
	entries, err := store.ReadExecHandoff(ctx, h.ID, h.ServiceID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial commit=%v err=%v", entries, err)
	}
}

func TestExecHandoffRejectsMemoryStore(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, ok := AsExecHandoffStore(store); ok {
		t.Fatal("in-memory store advertised durable exec handoff")
	}
}
