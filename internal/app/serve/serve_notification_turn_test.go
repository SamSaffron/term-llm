package serve

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func seedCompletionCandidate(t *testing.T, s *serveServer, store *session.SQLiteStore, responseID, sessionID, subscriptionID string) session.CompletionNotification {
	t.Helper()
	ctx := context.Background()
	if err := store.Create(ctx, &session.Session{ID: sessionID, Provider: "test", Model: "test", Origin: session.OriginWeb, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	origin := session.NotificationOrigin{SubscriptionID: subscriptionID, WindowID: "window_" + responseID, DocumentID: "document_" + responseID, Source: "/ui"}
	if err := store.UpdateNotificationPresence(ctx, session.NotificationPresence{Version: 2, WindowID: origin.WindowID, DocumentID: origin.DocumentID, SubscriptionID: subscriptionID, Source: origin.Source, Sequence: 1, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: responseID, SessionID: sessionID, OwnerInstanceID: "fixture", RunEpoch: 1, Turn: &session.NotificationTurnAdmission{Human: true, Origin: origin}})
	if err != nil {
		t.Fatal(err)
	}
	attention, err := store.FinalizeResponseRun(ctx, session.ResponseRunTerminal{ResponseID: responseID, OwnerInstanceID: "fixture", FencingToken: lease.FencingToken, Outcome: session.ResponseRunCompleted, FinalRev: 1})
	if err != nil {
		t.Fatal(err)
	}
	return session.CompletionNotification{EventID: "fixture", ResponseID: responseID, SubscriptionID: subscriptionID, WindowID: origin.WindowID, DocumentID: origin.DocumentID, Source: origin.Source, StoreInstanceID: attention.StoreInstanceID, SessionID: sessionID, AttentionSeq: attention.LatestAttentionSeq, RunEpoch: 1, Outcome: session.ResponseRunCompleted, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
}

func TestUserTurnClientHandoffsTwoNotFive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sid := session.NewID()
	if err := store.Create(ctx, &session.Session{ID: sid, Provider: "test", Model: "test", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sub, err := store.UpsertPushSubscription(ctx, &session.PushSubscription{Endpoint: "https://example.invalid/owner", KeyP256DH: "key", KeyAuth: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.UpsertPushSubscription(ctx, &session.PushSubscription{Endpoint: "https://example.invalid/other-device", KeyP256DH: "key", KeyAuth: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	origin := session.NotificationOrigin{SubscriptionID: sub.ID, WindowID: "window_owner", DocumentID: "document_owner", Source: "/ui"}
	if err := store.UpdateNotificationPresence(ctx, session.NotificationPresence{Version: 2, WindowID: origin.WindowID, DocumentID: origin.DocumentID, SubscriptionID: sub.ID, Source: origin.Source, Sequence: 1, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	s := &serveServer{store: store}
	previous := ""
	previousCall := ""
	for i, step := range []struct {
		id, tool string
		human    bool
	}{{"list", "media_list", true}, {"add", "media_add", false}, {"queued", "", false}, {"play", "media_play", true}, {"playing", "", false}} {
		admission := &session.NotificationTurnAdmission{Human: step.human, Origin: origin, PreviousResponseID: previous}
		if !step.human {
			admission.ClientResultIDs = []string{previousCall}
		}
		lease, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: step.id, SessionID: sid, OwnerInstanceID: "owner", RunEpoch: int64(i + 1), Turn: admission})
		if err != nil {
			t.Fatal(err)
		}
		run := newResponseRun(step.id, sid, previous, "test", time.Now().Unix(), nil)
		s.configureResponseRunLifecycle(run, store, "owner", lease)
		pending := []map[string]any{}
		if step.tool != "" {
			pending = append(pending, map[string]any{"call_id": "call_" + step.id, "name": step.tool, "arguments": "{}"})
		}
		// Commentary cannot make a client handoff a logical completion.
		if err := run.appendTextDeltaSegmentEvent(0, 0, "Working on it..."); err != nil {
			t.Fatal(err)
		}
		if err := run.complete(map[string]any{"response": map[string]any{"id": step.id, "pending_client_calls": pending}}, core.Usage{}, core.Usage{}); err != nil {
			t.Fatal(err)
		}
		if (run.attentionSeq > 0) != (step.tool == "") {
			t.Fatalf("%s attention=%d", step.id, run.attentionSeq)
		}
		previous = step.id
		previousCall = "call_" + step.id
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var candidates, wrongDevice, markers int
	if err := db.QueryRow(`SELECT count(*),coalesce(sum(subscription_id=?),0) FROM completion_push_outbox`, other.ID).Scan(&candidates, &wrongDevice); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM serve_response_lifecycle WHERE attention_seq>0`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if candidates != 2 || markers != 2 || wrongDevice != 0 {
		t.Fatalf("candidates=%d markers=%d other-device=%d; want2/2/0", candidates, markers, wrongDevice)
	}
}
