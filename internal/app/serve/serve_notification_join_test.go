package serve

import (
	"context"
	"errors"
	"testing"
	"time"

	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
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

// Queue steering after host-wake admission, then let the real engine incorporate
// it at the next tool boundary before the scripted provider failure.
type notificationSteeringProvider struct {
	*mockprovider.MockProvider
	steer func(context.Context) error
}

func (p *notificationSteeringProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	if p.CurrentTurn() == 0 {
		if err := p.steer(ctx); err != nil {
			return nil, err
		}
	}
	return p.MockProvider.Stream(ctx, req)
}

func TestNotificationHostWakeFailureRetryOwnership(t *testing.T) {
	for _, steering := range []bool{false, true} {
		name := "ordinary-host-retry"
		if steering {
			name = "incorporated-human-steering"
		}
		t.Run(name, func(t *testing.T) {
			provider := mockprovider.NewMockProvider("debug")
			if steering {
				provider.AddToolCall("boundary", "echo", map[string]any{"input": "continue"})
			}
			provider.AddError(errors.New("injected host synthesis failure")).AddTextResponse("recovered synthesis")
			s, store, rt := newAgentWakeFixture(t, provider)
			s.shutdownCh = make(chan struct{})
			s.responseLifecycleOnce.Do(func() {})
			t.Cleanup(func() { close(s.shutdownCh) })
			ctx := context.Background()
			origin := notificationWakeTestOrigin(t, store, "original")
			lease, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: "human", SessionID: "wake-parent", RunEpoch: 1, OwnerInstanceID: "fixture", Turn: &session.NotificationTurnAdmission{Human: true, Origin: origin}})
			if err != nil {
				t.Fatal(err)
			}
			child := session.AgentRun{ID: "child", ParentSessionID: "wake-parent", ParentResponseID: "human", RunGeneration: 3, AgentName: "developer", Status: "running", OwnerInstanceID: "fixture", NotifyWhenDone: true, NotifyOrigin: tools.QueueAgentOriginWeb}
			if err := store.PutAgentRun(ctx, child); err != nil {
				t.Fatal(err)
			}
			attention, err := store.FinalizeResponseRun(ctx, session.ResponseRunTerminal{ResponseID: "human", OwnerInstanceID: "fixture", FencingToken: lease.FencingToken, Outcome: session.ResponseRunCompleted, FinalRev: 1, AwaitingAgentJoin: true, AwaitedEvents: []session.AgentRun{child}})
			if err != nil || attention.Changed {
				t.Fatalf("source settled prematurely: %+v %v", attention, err)
			}
			child.Status = "completed"
			if err := store.PutAgentRun(ctx, child); err != nil {
				t.Fatal(err)
			}
			if steering {
				origin = notificationWakeTestOrigin(t, store, "steering")
				wrapped := &notificationSteeringProvider{MockProvider: provider, steer: func(ctx context.Context) error {
					_, _, err := rt.InterruptMessage(session.ContextWithNotificationOrigin(ctx, origin), core.UserText("change task during wake"), "change task during wake", "human-steering", nil, interruptDeliverySteer)
					return err
				}}
				rt.provider = wrapped
				registry := engine.NewToolRegistry()
				registry.Register(&echoTool{})
				rt.engine = engine.NewEngine(wrapped, registry)
			}
			if s.deliverAgentWake("wake-parent") {
				t.Fatal("provider failure did not request host retry")
			}
			s.responseRuns.mu.Lock()
			var failed *responseRun
			for _, run := range s.responseRuns.runs {
				failed = run
			}
			s.responseRuns.mu.Unlock()
			if failed == nil {
				t.Fatal("failed host response missing")
			}
			failedID := failed.id
			failed.mu.Lock()
			isHost, awaited := failed.recoverableNotification, len(failed.awaitedEvents)
			failed.mu.Unlock()
			if !isHost || awaited != 0 {
				t.Fatalf("not a real host retry: recoverable=%v per-run awaited=%d", isHost, awaited)
			}
			ack, err := store.GetAgentRun(ctx, child.ID)
			if err != nil || !ack.NotifiedAt.IsZero() {
				t.Fatalf("failed host wake ACKed child: %+v %v", ack, err)
			}
			attention, err = store.GetAttention(ctx, "wake-parent")
			if err != nil || (attention.LatestAttentionSeq > 0) != steering {
				t.Fatalf("wrong failure attention: %+v %v", attention, err)
			}
			failedSeq := attention.LatestAttentionSeq
			if steering {
				if attention.ResponseID != failedID || attention.Outcome != session.ResponseRunFailed {
					t.Fatalf("human failure not settled: %+v", attention)
				}
				requests := provider.RecordedRequests()
				incorporated := false
				for _, msg := range requests[len(requests)-1].Messages {
					incorporated = incorporated || msg.ClientMessageID == "human-steering"
				}
				pending, err := store.ListPendingSteering(ctx, "wake-parent")
				if !incorporated || err != nil || len(pending) != 0 {
					t.Fatalf("steering not durably incorporated: provider=%v pending=%+v err=%v", incorporated, pending, err)
				}
			} else {
				rows, err := store.ClaimCompletionNotifications(ctx, "intermediate", time.Now().Add(4*time.Second), time.Minute, 10)
				if err != nil || len(rows) != 0 {
					t.Fatalf("intermediate host candidate: %+v %v", rows, err)
				}
			}
			if !s.deliverAgentWake("wake-parent") {
				t.Fatal("host retry failed")
			}
			ack, err = store.GetAgentRun(ctx, child.ID)
			if err != nil || ack.NotifiedAt.IsZero() || ack.RunGeneration != child.RunGeneration {
				t.Fatalf("successful retry did not ACK exact child: %+v %v", ack, err)
			}
			attention, err = store.GetAttention(ctx, "wake-parent")
			if err != nil {
				t.Fatal(err)
			}
			rows, err := store.ClaimCompletionNotifications(ctx, "final", time.Now().Add(4*time.Second), time.Minute, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("final candidates=%+v err=%v", rows, err)
			}
			wantResponse, wantOutcome := attention.ResponseID, session.ResponseRunCompleted
			if !steering && (wantResponse == failedID || wantResponse == "human") {
				t.Fatalf("host retry failed to settle its own response: %+v", attention)
			}
			if steering {
				wantResponse, wantOutcome = failedID, session.ResponseRunFailed
			}
			if n := rows[0]; n.ResponseID != wantResponse || n.Outcome != wantOutcome || n.SubscriptionID != origin.SubscriptionID || n.WindowID != origin.WindowID || n.DocumentID != origin.DocumentID || n.Source != origin.Source || n.AttentionSeq == 0 || n.RunEpoch == 0 || (steering && n.AttentionSeq != failedSeq) {
				t.Fatalf("retry lost exact logical owner: %+v", n)
			}
			wantTurns := 2
			if steering {
				wantTurns++
			}
			if provider.CurrentTurn() != wantTurns {
				t.Fatalf("provider turns=%d want=%d", provider.CurrentTurn(), wantTurns)
			}
		})
	}
}

func notificationWakeTestOrigin(t *testing.T, store *session.SQLiteStore, suffix string) session.NotificationOrigin {
	t.Helper()
	ctx := context.Background()
	sub, err := store.UpsertPushSubscription(ctx, &session.PushSubscription{Endpoint: "https://example.invalid/" + suffix, KeyP256DH: "key", KeyAuth: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	origin := session.NotificationOrigin{SubscriptionID: sub.ID, WindowID: "window_" + suffix, DocumentID: "document_" + suffix, Source: "/ui"}
	if err := store.UpdateNotificationPresence(ctx, session.NotificationPresence{Version: 2, WindowID: origin.WindowID, DocumentID: origin.DocumentID, SubscriptionID: sub.ID, Source: origin.Source, Sequence: 1, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return origin
}
