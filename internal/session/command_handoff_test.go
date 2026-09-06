package session

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCommandHandoffExplicitOneShot(t *testing.T) {
	for _, variant := range []string{"consume", "same instance", "wrong service", "advanced", "discarded"} {
		t.Run(variant, func(t *testing.T) {
			store, ctx, sid := newAttentionTestStore(t)
			h := CommandHandoff{ID: "restart-id", Service: "chat-service", SourceInstance: "old", SessionID: sid, Payload: json.RawMessage(`{"draft":"unsent","continue":true}`)}
			if err := store.SaveCommandHandoff(ctx, h); err != nil {
				t.Fatal(err)
			}
			instance, service := "new", h.Service
			switch variant {
			case "same instance":
				instance = "old"
			case "wrong service":
				service = "another"
			case "advanced":
				if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET transcript_rev=transcript_rev+1 WHERE id=?`, sid); err != nil {
					t.Fatal(err)
				}
			case "discarded":
				if err := store.DiscardCommandHandoff(ctx, h.ID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := store.ConsumeCommandHandoff(ctx, h.ID, service, instance)
			if variant != "consume" {
				if !errors.Is(err, ErrExecHandoffConflict) {
					t.Fatalf("invalid claim accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Payload) != string(h.Payload) || got.SessionID != sid {
				t.Fatalf("lost payload: %+v", got)
			}
			if _, err := store.ConsumeCommandHandoff(ctx, h.ID, service, "third"); !errors.Is(err, ErrExecHandoffConflict) {
				t.Fatalf("claim replayed: %v", err)
			}
		})
	}
}

func TestCommandHandoffReclaimOnlyOriginalInstance(t *testing.T) {
	for _, variant := range []string{"original", "other-instance", "wrong-service", "advanced", "expired"} {
		t.Run(variant, func(t *testing.T) {
			store, ctx, sid := newAttentionTestStore(t)
			h := CommandHandoff{ID: "failed-replacement", Service: "jobs", SourceInstance: "source", SessionID: sid, Payload: json.RawMessage(`{}`)}
			if err := store.SaveCommandHandoff(ctx, h); err != nil {
				t.Fatal(err)
			}
			source, service := "source", "jobs"
			switch variant {
			case "other-instance":
				source = "replacement"
			case "wrong-service":
				service = "other"
			case "advanced":
				if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET transcript_rev=transcript_rev+1 WHERE id=?`, sid); err != nil {
					t.Fatal(err)
				}
			case "expired":
				var raw string
				if err := store.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, "process_handoff:"+h.ID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(raw), &h); err != nil {
					t.Fatal(err)
				}
				h.Created = h.Created.Add(-10 * time.Minute)
				payload, _ := json.Marshal(h)
				if _, err := store.db.ExecContext(ctx, `UPDATE metadata SET value=? WHERE key=?`, string(payload), "process_handoff:"+h.ID); err != nil {
					t.Fatal(err)
				}
			}
			_, err := store.ReclaimCommandHandoff(ctx, h.ID, service, source)
			if variant != "original" {
				if !errors.Is(err, ErrExecHandoffConflict) {
					t.Fatalf("invalid reclaim: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.ReclaimCommandHandoff(ctx, h.ID, service, source); !errors.Is(err, ErrExecHandoffConflict) {
				t.Fatalf("reclaim replayed: %v", err)
			}
			if _, err = store.ConsumeCommandHandoff(ctx, h.ID, service, "new"); !errors.Is(err, ErrExecHandoffConflict) {
				t.Fatalf("reclaimed grant adopted again: %v", err)
			}
		})
	}
}
