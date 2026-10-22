package serve

import (
	"context"
	"testing"
	"time"

	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestNotificationHostWakeFinishesOnlyExplicitAwaitedTurn(t *testing.T) {
	for _, awaited := range []bool{false, true} {
		name := "detached"
		if awaited {
			name = "awaited"
		}
		t.Run(name, func(t *testing.T) {
			provider := mockprovider.NewMockProvider("debug").AddTextResponse("Final synthesis from child results")
			s, store, _ := newAgentWakeFixture(t, provider)
			s.shutdownCh = make(chan struct{})
			// Exercise one explicit delivery, not a competing startup reconciler.
			s.responseLifecycleOnce.Do(func() {})
			t.Cleanup(func() { close(s.shutdownCh) })
			s.cfgRef.Serve.WebPush.VAPIDPublicKey = "test-key"
			s.cfgRef.Serve.WebPush.VAPIDPrivateKey = "test-private"
			ctx := context.Background()
			sub, err := store.UpsertPushSubscription(ctx, &session.PushSubscription{Endpoint: "https://example.invalid/never-sent", KeyP256DH: "key", KeyAuth: "auth", VAPIDKeyID: appnotify.WebPushKeyID("test-key")})
			if err != nil {
				t.Fatal(err)
			}
			origin := session.NotificationOrigin{SubscriptionID: sub.ID, WindowID: "window_owner", DocumentID: "document_owner", Source: "/ui"}
			if err := store.UpdateNotificationPresence(ctx, session.NotificationPresence{Version: 2, WindowID: origin.WindowID, DocumentID: origin.DocumentID, SubscriptionID: sub.ID, Source: origin.Source, Sequence: 1, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			lease, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: "human", SessionID: "wake-parent", RunEpoch: 1, OwnerInstanceID: "human-fixture", Turn: &session.NotificationTurnAdmission{Human: true, Origin: origin}})
			if err != nil {
				t.Fatal(err)
			}
			child := session.AgentRun{ID: "child", ParentSessionID: "wake-parent", ParentResponseID: "human", RunGeneration: 1, AgentName: "developer", Status: "running", OwnerInstanceID: "fixture", NotifyWhenDone: true, NotifyOrigin: tools.QueueAgentOriginWeb}
			if err := store.PutAgentRun(ctx, child); err != nil {
				t.Fatal(err)
			}
			terminal := session.ResponseRunTerminal{ResponseID: "human", OwnerInstanceID: "human-fixture", FencingToken: lease.FencingToken, Outcome: session.ResponseRunCompleted, FinalRev: 1}
			if awaited {
				terminal.AwaitingAgentJoin = true
				terminal.AwaitedEvents = []session.AgentRun{child}
			}
			if _, err := store.FinalizeResponseRun(ctx, terminal); err != nil {
				t.Fatal(err)
			}
			child.Status = "completed"
			if err := store.PutAgentRun(ctx, child); err != nil {
				t.Fatal(err)
			}
			if !s.deliverAgentWake("wake-parent") {
				t.Fatal("host synthesis failed")
			}
			rows, err := store.ClaimCompletionNotifications(ctx, "test", time.Now().Add(4*time.Second), 30*time.Second, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("candidates=%+v err=%v", rows, err)
			}
			if (rows[0].ResponseID != "human") != awaited {
				t.Fatalf("unproven notification inheritance: awaited=%v response=%s", awaited, rows[0].ResponseID)
			}
			ack, err := store.GetAgentRun(ctx, child.ID)
			if err != nil || ack.NotifiedAt.IsZero() {
				t.Fatalf("generation not durably acknowledged: %+v %v", ack, err)
			}
			if provider.CurrentTurn() != 1 {
				t.Fatalf("unexpected inference count %d", provider.CurrentTurn())
			}
		})
	}
}
