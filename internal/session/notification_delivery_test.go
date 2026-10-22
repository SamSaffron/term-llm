package session

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func completedNotificationTestRun(t *testing.T) (*SQLiteStore, NotificationOrigin, CompletionNotification) {
	t.Helper()
	s, ctx, sid := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	l := admitNotificationTestRun(t, s, ctx, sid, "response", &NotificationTurnAdmission{Human: true, Origin: o})
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	items, err := s.ClaimCompletionNotifications(ctx, "worker", time.Now().Add(3*time.Second), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim=%+v %v", items, err)
	}
	return s, o, items[0]
}

func TestNotificationPresenceOrderingDocumentsAndLeaseBounds(t *testing.T) {
	s, ctx, _ := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	p := NotificationPresence{Version: 2, Source: o.Source, WindowID: o.WindowID, DocumentID: o.DocumentID, SubscriptionID: o.SubscriptionID, Sequence: 2, Active: true, ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.UpdateNotificationPresence(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPresence(ctx, p); !errors.Is(err, ErrNotificationPresenceConflict) {
		t.Fatalf("reordered sequence=%v", err)
	}
	sibling := p
	sibling.DocumentID = "clone"
	sibling.Sequence = 1
	if err := s.UpdateNotificationPresence(ctx, sibling); !errors.Is(err, ErrNotificationPresenceConflict) {
		t.Fatalf("live clone stole window: %v", err)
	}
	p.Sequence++
	p.Active = false
	if err := s.UpdateNotificationPresence(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPresence(ctx, sibling); err != nil {
		t.Fatalf("new document after inactive failed: %v", err)
	}
	p.Sequence++
	if err := s.UpdateNotificationPresence(ctx, p); !errors.Is(err, ErrNotificationPresenceConflict) {
		t.Fatalf("retired document reclaimed binding: %v", err)
	}
	for _, bad := range []NotificationPresence{
		{Version: 2, Source: o.Source, WindowID: "w", DocumentID: "d", SubscriptionID: o.SubscriptionID, Sequence: 1, ExpiresAt: time.Now().Add(66 * time.Second)},
		{Version: 2, Source: o.Source, WindowID: "w", DocumentID: "d", SubscriptionID: o.SubscriptionID, Sequence: 1, ExpiresAt: time.Now().Add(-time.Second)},
		{Version: 2, Source: "/../bad", WindowID: "w", DocumentID: "d", SubscriptionID: o.SubscriptionID, Sequence: 1, ExpiresAt: time.Now().Add(time.Minute)},
	} {
		if err := s.UpdateNotificationPresence(ctx, bad); err == nil {
			t.Fatalf("accepted invalid presence: %+v", bad)
		}
	}
}

func TestNotificationPresenceDefersWithoutReadingAndRechecksAfterClaim(t *testing.T) {
	s, o, n := completedNotificationTestRun(t)
	ctx := t.Context()
	p := NotificationPresence{Version: 2, Source: o.Source, WindowID: o.WindowID, DocumentID: o.DocumentID, SubscriptionID: o.SubscriptionID, Sequence: 2, Active: true, ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.UpdateNotificationPresence(ctx, p); err != nil {
		t.Fatal(err)
	}
	ready, err := s.RecheckCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, time.Now().Add(3*time.Second))
	if err != nil || ready {
		t.Fatalf("owner attended but ready=%v %v", ready, err)
	}
	if notificationCount(t, s, "pending") != 1 {
		t.Fatal("presence killed unread candidate")
	}
	a, err := s.GetAttention(ctx, n.SessionID)
	if err != nil || a.SeenThroughSeq != 0 || !a.Unseen {
		t.Fatalf("presence manufactured read: %+v %v", a, err)
	}
	items, err := s.ClaimCompletionNotifications(ctx, "other-worker", time.Now().Add(4*time.Second), time.Minute, 1)
	if err != nil || len(items) != 0 {
		t.Fatalf("attended claim=%+v %v", items, err)
	}
	p.Sequence++
	p.Active = false
	if err = s.UpdateNotificationPresence(ctx, p); err != nil {
		t.Fatal(err)
	}
	items, err = s.ClaimCompletionNotifications(ctx, "other-worker", time.Now().Add(4*time.Second), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("unattended claim=%+v %v", items, err)
	}
	if err = s.CompleteCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, "delivered", time.Time{}, ""); !errors.Is(err, ErrNotificationClaimLost) {
		t.Fatalf("released worker completed successor: %v", err)
	}
}

func TestNotificationClaimAtomicAcrossStoreConnectionsAndResultsFenced(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	l := admitNotificationTestRun(t, s, ctx, sid, "response", &NotificationTurnAdmission{Human: true, Origin: o})
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	other, err := NewSQLiteStore(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stores := []*SQLiteStore{s, other}
	results := make(chan []CompletionNotification, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func(i int, store *SQLiteStore) {
			defer wg.Done()
			items, err := store.ClaimCompletionNotifications(ctx, string(rune('a'+i)), time.Now().Add(3*time.Second), time.Minute, 25)
			results <- items
			errs <- err
		}(i, store)
	}
	wg.Wait()
	close(results)
	close(errs)
	var claimed []CompletionNotification
	for items := range results {
		claimed = append(claimed, items...)
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(claimed) != 1 {
		t.Fatalf("concurrent claims=%+v", claimed)
	}
	n := claimed[0]
	if err = s.CompleteCompletionNotification(ctx, n.ID, "wrong", n.ClaimToken, "delivered", time.Time{}, ""); !errors.Is(err, ErrNotificationClaimLost) {
		t.Fatalf("wrong owner result=%v", err)
	}
	if err = s.CompleteCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, "retry", time.Now(), "retryable"); err != nil {
		t.Fatal(err)
	}
	items, err := s.ClaimCompletionNotifications(ctx, "successor", time.Now().Add(3*time.Second), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("retry=%+v %v", items, err)
	}
	next := items[0]
	if next.EventID != n.EventID || next.ClaimToken == n.ClaimToken || next.AttemptCount != 1 {
		t.Fatalf("retry identity/fence=%+v", next)
	}
	if err = s.CompleteCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, "dead", time.Time{}, ""); !errors.Is(err, ErrNotificationClaimLost) {
		t.Fatalf("stale result=%v", err)
	}
	if err = s.CompleteCompletionNotification(ctx, next.ID, next.ClaimOwner, next.ClaimToken, "defer", time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	items, err = s.ClaimCompletionNotifications(ctx, "last", time.Now().Add(3*time.Second), time.Minute, 1)
	if err != nil || len(items) != 1 || items[0].AttemptCount != 1 {
		t.Fatalf("defer incremented attempt=%+v %v", items, err)
	}
	next = items[0]
	if err = s.CompleteCompletionNotification(ctx, next.ID, next.ClaimOwner, next.ClaimToken, "delivered", time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, s, "delivered") != 1 {
		t.Fatal("delivery not committed")
	}
}

func TestNotificationRechecksReadSupersessionOptoutAndExpiry(t *testing.T) {
	for _, mode := range []string{"read", "read-then-unseen", "superseded", "delete-id", "delete-endpoint", "stale", "expired", "store-replaced"} {
		t.Run(mode, func(t *testing.T) {
			s, o, n := completedNotificationTestRun(t)
			ctx := t.Context()
			switch mode {
			case "read", "read-then-unseen":
				if _, err := s.MarkAttentionSeen(ctx, n.SessionID, n.StoreInstanceID, n.AttentionSeq); err != nil {
					t.Fatal(err)
				}
				if mode == "read-then-unseen" {
					if _, err := s.MarkAttentionUnseen(ctx, n.SessionID, n.StoreInstanceID, n.AttentionSeq); err != nil {
						t.Fatal(err)
					}
				}
			case "superseded":
				admitNotificationTestRun(t, s, ctx, n.SessionID, "new-human", &NotificationTurnAdmission{Human: true})
			case "delete-id":
				if err := s.DeletePushSubscriptionByID(ctx, o.SubscriptionID); err != nil {
					t.Fatal(err)
				}
			case "delete-endpoint":
				sub, _ := s.GetPushSubscription(ctx, o.SubscriptionID)
				if err := s.DeletePushSubscription(ctx, sub.Endpoint); err != nil {
					t.Fatal(err)
				}
			case "stale":
				if err := s.MarkPushSubscriptionStale(ctx, o.SubscriptionID, "410", "gone"); err != nil {
					t.Fatal(err)
				}
				if err := s.MarkPushSubscriptionUsed(ctx, o.SubscriptionID); err != nil {
					t.Fatal(err)
				}
				sub, _ := s.GetPushSubscription(ctx, o.SubscriptionID)
				if sub.Status != "stale" {
					t.Fatal("MarkUsed resurrected stale subscription")
				}
			case "store-replaced":
				if _, err := s.db.Exec(`UPDATE metadata SET value='replacement-store' WHERE key='store_instance_id'`); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := s.db.Exec(`UPDATE completion_push_outbox SET expires_at=1 WHERE id=?`, n.ID); err != nil {
					t.Fatal(err)
				}
			}
			ready, err := s.RecheckCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, time.Now().Add(3*time.Second))
			if ready || err != nil && !errors.Is(err, ErrNotificationClaimLost) {
				t.Fatalf("invalid candidate ready=%v error=%v", ready, err)
			}
			if notificationCount(t, s, "pending") != 0 {
				t.Fatal("invalid candidate retained")
			}
			if err = s.CompleteCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, "delivered", time.Time{}, ""); !errors.Is(err, ErrNotificationClaimLost) {
				t.Fatalf("retired result accepted: %v", err)
			}
		})
	}
}

func TestNotificationExpiredClaimCannotSettleReplacement(t *testing.T) {
	s, _, n := completedNotificationTestRun(t)
	ctx := t.Context()
	if _, err := s.db.Exec(`UPDATE completion_push_outbox SET claim_expires_at=0 WHERE id=?`, n.ID); err != nil {
		t.Fatal(err)
	}
	items, err := s.ClaimCompletionNotifications(ctx, "replacement", time.Now().Add(3*time.Second), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("reclaim=%+v %v", items, err)
	}
	if err = s.CompleteCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, "delivered", time.Time{}, ""); !errors.Is(err, ErrNotificationClaimLost) {
		t.Fatalf("expired owner result=%v", err)
	}
}

func TestNotificationSubscriptionRetirementRollsBackWithDelete(t *testing.T) {
	s, o, n := completedNotificationTestRun(t)
	ctx := t.Context()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_subscription_delete AFTER DELETE ON push_subscriptions BEGIN SELECT RAISE(ABORT,'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePushSubscriptionByID(ctx, o.SubscriptionID); err == nil {
		t.Fatal("expected delete transaction failure")
	}
	sub, err := s.GetPushSubscription(ctx, o.SubscriptionID)
	if err != nil || sub == nil || sub.Status != "active" || notificationCount(t, s, "pending") != 1 {
		t.Fatalf("retirement escaped delete rollback: %+v %v", sub, err)
	}
	var bindings int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM notification_presence WHERE subscription_id=?`, o.SubscriptionID).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("binding escaped rollback: %d %v", bindings, err)
	}
	ready, err := s.RecheckCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, time.Now().Add(3*time.Second))
	if err != nil || !ready {
		t.Fatalf("rolled-back claim retired: %v %v", ready, err)
	}
}

func TestNotificationPresenceActiveReloadUsesExactIncarnationCAS(t *testing.T) {
	s, ctx, _ := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	old := NotificationPresence{Version: 2, Source: o.Source, WindowID: o.WindowID, DocumentID: o.DocumentID, SubscriptionID: o.SubscriptionID, Sequence: 2, Active: true, ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.UpdateNotificationPresence(ctx, old); err != nil {
		t.Fatal(err)
	}
	next := old
	next.DocumentID = "fresh-doc"
	next.Sequence = 1
	next.PreviousDocumentID = "wrong-predecessor"
	if err := s.UpdateNotificationPresence(ctx, next); !errors.Is(err, ErrNotificationPresenceConflict) {
		t.Fatalf("wrong CAS replaced live owner: %v", err)
	}
	next.PreviousDocumentID = old.DocumentID
	if err := s.UpdateNotificationPresence(ctx, next); err != nil {
		t.Fatalf("exact reload CAS rejected: %v", err)
	}
	old.Sequence++
	if err := s.UpdateNotificationPresence(ctx, old); !errors.Is(err, ErrNotificationPresenceConflict) {
		t.Fatalf("retired heartbeat reclaimed window: %v", err)
	}
	old.PreviousDocumentID = next.DocumentID
	if err := s.UpdateNotificationPresence(ctx, old); !errors.Is(err, ErrNotificationPresenceConflict) {
		t.Fatalf("retired document used CAS to reclaim: %v", err)
	}
	next.Sequence = 0
	if err := s.UpdateNotificationPresence(ctx, next); err == nil {
		t.Fatal("reordered reload heartbeat accepted")
	}
}

func TestNotificationExplicitReenrollmentCanRebindSameDocument(t *testing.T) {
	s, ctx, _ := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	if err := s.DeletePushSubscriptionByID(ctx, o.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	p := NotificationPresence{Version: 2, Source: o.Source, WindowID: o.WindowID, DocumentID: o.DocumentID, SubscriptionID: o.SubscriptionID, Sequence: 2, ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.UpdateNotificationPresence(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old subscription resurrected: %v", err)
	}
	sub, err := s.UpsertPushSubscription(ctx, &PushSubscription{Endpoint: "https://push.invalid/owner", KeyP256DH: "new-key", KeyAuth: "new-auth"})
	if err != nil {
		t.Fatal(err)
	}
	p.SubscriptionID = sub.ID
	if err = s.UpdateNotificationPresence(ctx, p); err != nil {
		t.Fatalf("explicit enable failed same document: %v", err)
	}
}

func TestNotificationLegacyOutboxCannotConsumeFencedCandidates(t *testing.T) {
	s, _, n := completedNotificationTestRun(t)
	ctx := t.Context()
	due, err := s.ListDueCompletionPushes(ctx, time.Now().Add(time.Minute), 100)
	if err != nil || len(due) != 0 {
		t.Fatalf("legacy listed fenced candidate: %+v %v", due, err)
	}
	for _, mark := range []func() error{func() error { return s.MarkCompletionPushDelivered(ctx, n.ID) }, func() error { return s.MarkCompletionPushDead(ctx, n.ID, "legacy") }, func() error { return s.RetryCompletionPush(ctx, n.ID, time.Now(), "legacy") }} {
		if err := mark(); err != nil {
			t.Fatal(err)
		}
	}
	ready, err := s.RecheckCompletionNotification(ctx, n.ID, n.ClaimOwner, n.ClaimToken, time.Now().Add(3*time.Second))
	if err != nil || !ready {
		t.Fatalf("unfenced API mutated claim: %v %v", ready, err)
	}
	if _, ok := AsNotificationDeliveryStore(&LoggingStore{Store: s}); !ok {
		t.Fatal("logging wrapper hid delivery capability")
	}
	turns, ok := AsNotificationTurnStore(&LoggingStore{Store: s})
	if !ok {
		t.Fatal("logging wrapper hid turn capability")
	}
	if err := turns.CancelNotificationTurn(ctx, "not-a-response"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cancel=%v", err)
	}
}
