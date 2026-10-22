package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
)

func notificationTestOrigin(t *testing.T, s *SQLiteStore, ctx context.Context, suffix string) NotificationOrigin {
	t.Helper()
	sub, err := s.UpsertPushSubscription(ctx, &PushSubscription{Endpoint: "https://push.invalid/" + suffix, KeyP256DH: "key", KeyAuth: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	origin := NotificationOrigin{SubscriptionID: sub.ID, WindowID: "window-" + suffix, DocumentID: "doc-" + suffix, Source: "/node/Test"}
	if err = s.UpdateNotificationPresence(ctx, NotificationPresence{Version: 2, Source: origin.Source, SubscriptionID: sub.ID, WindowID: origin.WindowID, DocumentID: origin.DocumentID, Sequence: 1, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return origin
}
func admitNotificationTestRun(t *testing.T, s *SQLiteStore, ctx context.Context, sessionID, responseID string, turn *NotificationTurnAdmission, events ...AgentRun) ResponseRunLease {
	t.Helper()
	lease, err := s.AdmitResponseRun(ctx, ResponseRunAdmission{SessionID: sessionID, ResponseID: responseID, OwnerInstanceID: "owner", RunEpoch: 7, Turn: turn, AgentEvents: events})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
func terminalNotificationTestRun(t *testing.T, s *SQLiteStore, ctx context.Context, lease ResponseRunLease, outcome ResponseRunState, extra func(*ResponseRunTerminal)) AttentionState {
	t.Helper()
	terminal := ResponseRunTerminal{ResponseID: lease.ResponseID, OwnerInstanceID: "owner", FencingToken: lease.FencingToken, Outcome: outcome, FinalRev: 2, DurableOutputCount: 1}
	if extra != nil {
		extra(&terminal)
	}
	a, err := s.FinalizeResponseRun(ctx, terminal)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func notificationCount(t *testing.T, s *SQLiteStore, status string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM completion_push_outbox WHERE status=?`, status).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestNotificationTwoLogicalCompletionsNotFiveProtocolHops(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	origin := notificationTestOrigin(t, s, ctx, "owner")
	var previous string
	for i, step := range []struct {
		human, pending bool
		id             string
	}{{true, true, "media_list"}, {false, true, "media_add"}, {false, false, "first_final"}, {true, true, "media_play"}, {false, false, "second_final"}} {
		turn := &NotificationTurnAdmission{Human: step.human, Origin: origin, PreviousResponseID: previous}
		if !step.human {
			turn.ClientResultIDs = []string{previous + "-call"}
			turn.Origin = NotificationOrigin{SubscriptionID: "do-not-resample"}
		}
		lease := admitNotificationTestRun(t, s, ctx, sid, step.id, turn)
		a := terminalNotificationTestRun(t, s, ctx, lease, ResponseRunCompleted, func(v *ResponseRunTerminal) {
			v.PendingClientCalls = step.pending
			if step.pending {
				v.PendingClientCallIDs = []string{step.id + "-call"}
			}
		})
		if a.Changed == step.pending {
			t.Fatalf("step %d intermediate=%v attention=%+v", i, step.pending, a)
		}
		previous = step.id
	}
	if got := notificationCount(t, s, "pending"); got != 1 {
		t.Fatalf("current candidate count=%d", got)
	}
	var total, markers int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM completion_push_outbox`).Scan(&total)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM serve_response_lifecycle WHERE attention_seq>0`).Scan(&markers)
	if total != 2 || markers != 2 {
		t.Fatalf("candidates=%d final attention=%d, want two not five", total, markers)
	}
	items, err := s.ClaimCompletionNotifications(ctx, "worker", time.Now().Add(3*time.Second), time.Minute, 25)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim=%+v %v", items, err)
	}
	n := items[0]
	if n.SubscriptionID != origin.SubscriptionID || n.WindowID != origin.WindowID || n.Source != origin.Source || n.ResponseID != "second_final" || n.AttentionSeq == 0 || n.RunEpoch != 7 || n.StoreInstanceID == "" || n.SessionID != sid || n.EventID != "completion:"+n.TurnID+":"+origin.SubscriptionID {
		t.Fatalf("candidate lost exact identity: %+v", n)
	}
	if delta := n.ExpiresAt.Sub(n.CreatedAt); delta != 24*time.Hour {
		t.Fatalf("expiry=%v", delta)
	}
}

func TestNotificationClientResultsMustExactlyMatchPendingCalls(t *testing.T) {
	for _, ids := range [][]string{nil, {"one"}, {"one", "one"}, {"one", "wrong"}, {"one", "two", "extra"}, {"two", "one"}} {
		t.Run(fmt.Sprint(ids), func(t *testing.T) {
			s, ctx, sid := newAttentionTestStore(t)
			o := notificationTestOrigin(t, s, ctx, "owner")
			l := admitNotificationTestRun(t, s, ctx, sid, "source", &NotificationTurnAdmission{Human: true, Origin: o})
			terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, func(v *ResponseRunTerminal) {
				v.PendingClientCalls = true
				v.PendingClientCallIDs = []string{"one", "two"}
			})
			_, err := s.AdmitResponseRun(ctx, ResponseRunAdmission{SessionID: sid, ResponseID: "next", OwnerInstanceID: "owner", Turn: &NotificationTurnAdmission{PreviousResponseID: "source", ClientResultIDs: ids}})
			valid := len(ids) == 2 && ids[0] == "two"
			if valid && err != nil || !valid && !errors.Is(err, ErrNotificationLineage) {
				t.Fatalf("ids=%v err=%v", ids, err)
			}
		})
	}
}

func TestNotificationHumanNoDestinationSupersedesAndInternalDoesNotInherit(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	l := admitNotificationTestRun(t, s, ctx, sid, "human", &NotificationTurnAdmission{Human: true, Origin: o})
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	l = admitNotificationTestRun(t, s, ctx, sid, "autonomous", nil)
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	if notificationCount(t, s, "pending") != 1 {
		t.Fatal("internal completion acquired or retired authority")
	}
	l = admitNotificationTestRun(t, s, ctx, sid, "tui-human", &NotificationTurnAdmission{Human: true})
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	if notificationCount(t, s, "pending") != 0 {
		t.Fatal("destinationless human failed to supersede")
	}
}

func TestNotificationUnknownOrWrongSourceOriginFailsClosed(t *testing.T) {
	for _, change := range []func(*NotificationOrigin){func(o *NotificationOrigin) { o.DocumentID = "old" }, func(o *NotificationOrigin) { o.Source = "/wrong" }, func(o *NotificationOrigin) { o.WindowID = "other" }, func(o *NotificationOrigin) { o.SubscriptionID = "unknown" }} {
		s, ctx, sid := newAttentionTestStore(t)
		o := notificationTestOrigin(t, s, ctx, "owner")
		change(&o)
		l := admitNotificationTestRun(t, s, ctx, sid, "human", &NotificationTurnAdmission{Human: true, Origin: o})
		terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
		if notificationCount(t, s, "pending") != 0 {
			t.Fatal("invalid binding acquired authority")
		}
	}
}

func putNotificationChild(t *testing.T, s *SQLiteStore, ctx context.Context, sid, parent, id string, generation int64, status string) AgentRun {
	t.Helper()
	event := AgentRun{ID: id, ParentSessionID: sid, ParentResponseID: parent, RunGeneration: generation, Status: status, OwnerInstanceID: "child-owner", UpdatedAt: time.Now(), NotifyWhenDone: true, NotifyOrigin: "serve"}
	if err := s.PutAgentRun(ctx, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestNotificationAwaitedGenerationJoinAndRecoverableRetry(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	l := admitNotificationTestRun(t, s, ctx, sid, "human", &NotificationTurnAdmission{Human: true, Origin: o})
	a := putNotificationChild(t, s, ctx, sid, "human", "child-a", 2, "running")
	b := putNotificationChild(t, s, ctx, sid, "human", "child-b", 3, "running")
	state := terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, func(v *ResponseRunTerminal) { v.AwaitingAgentJoin = true; v.AwaitedEvents = []AgentRun{a, b} })
	if state.Changed || notificationCount(t, s, "pending") != 0 {
		t.Fatal("awaiting source completed early")
	}
	a = putNotificationChild(t, s, ctx, sid, "human", a.ID, 2, "completed")
	// Host predecessor is intentionally irrelevant: only exact awaited events select.
	l = admitNotificationTestRun(t, s, ctx, sid, "wake-failed", &NotificationTurnAdmission{PreviousResponseID: "not-a-client-chain", AwaitedEvents: []AgentRun{a}}, a)
	state = terminalNotificationTestRun(t, s, ctx, l, ResponseRunFailed, func(v *ResponseRunTerminal) { v.Recoverable = true; v.AgentEvents = []AgentRun{a} })
	got, err := s.GetAgentRun(ctx, a.ID)
	if err != nil || !got.NotifiedAt.IsZero() || state.Changed {
		t.Fatalf("recoverable finalized/ACKed: %+v %+v %v", state, got, err)
	}
	l = admitNotificationTestRun(t, s, ctx, sid, "wake-a", &NotificationTurnAdmission{AwaitedEvents: []AgentRun{a}}, a)
	state = terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	got, err = s.GetAgentRun(ctx, a.ID)
	if err != nil || got.NotifiedAt.IsZero() || state.Changed {
		t.Fatalf("remaining join lost or ACK missing: %+v %+v %v", state, got, err)
	}
	b = putNotificationChild(t, s, ctx, sid, "human", b.ID, 3, "completed")
	l = admitNotificationTestRun(t, s, ctx, sid, "wake-b", &NotificationTurnAdmission{AwaitedEvents: []AgentRun{b}})
	state = terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	if !state.Changed || notificationCount(t, s, "pending") != 1 {
		t.Fatalf("joined final failed: %+v", state)
	}
}

func TestNotificationDetachedOrUnprovenAgentNeverInherits(t *testing.T) {
	for _, mode := range []string{"detached", "generation_changed", "wrong_parent", "collected"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, sid := newAttentionTestStore(t)
			o := notificationTestOrigin(t, s, ctx, "owner")
			l := admitNotificationTestRun(t, s, ctx, sid, "human", &NotificationTurnAdmission{Human: true, Origin: o})
			a := putNotificationChild(t, s, ctx, sid, "human", "child", 1, "running")
			terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, func(v *ResponseRunTerminal) {
				if mode != "detached" {
					v.AwaitingAgentJoin = true
					v.AwaitedEvents = []AgentRun{a}
				}
			})
			generation := int64(1)
			parent := "human"
			if mode == "generation_changed" {
				generation = 2
			}
			if mode == "wrong_parent" {
				parent = "other-response"
			}
			a = putNotificationChild(t, s, ctx, sid, parent, "child", generation, "completed")
			if mode == "collected" {
				if err := s.CollectAgentRunGeneration(ctx, a.ID, a.RunGeneration, time.Now()); err != nil {
					t.Fatal(err)
				}
				return
			}
			l = admitNotificationTestRun(t, s, ctx, sid, "wake", &NotificationTurnAdmission{PreviousResponseID: "human", AwaitedEvents: []AgentRun{a}}, a)
			terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
			var linked int
			_ = s.db.QueryRow(`SELECT COUNT(*) FROM notification_response_turns WHERE response_id='wake'`).Scan(&linked)
			if linked != 0 {
				t.Fatal("unproven event inherited turn")
			}
			expected := 0
			if mode == "detached" {
				expected = 1
			}
			if notificationCount(t, s, "pending") != expected {
				t.Fatal("unproven wake minted automatic notification")
			}
		})
	}
}

func TestNotificationFinalizationCandidateFailureRollsBackAttentionAndGenerationACK(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	l := admitNotificationTestRun(t, s, ctx, sid, "human", &NotificationTurnAdmission{Human: true, Origin: o})
	a := putNotificationChild(t, s, ctx, sid, "human", "child", 1, "running")
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, func(v *ResponseRunTerminal) { v.AwaitingAgentJoin = true; v.AwaitedEvents = []AgentRun{a} })
	a = putNotificationChild(t, s, ctx, sid, "human", "child", 1, "completed")
	l = admitNotificationTestRun(t, s, ctx, sid, "wake", &NotificationTurnAdmission{AwaitedEvents: []AgentRun{a}}, a)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_notification BEFORE INSERT ON completion_push_outbox BEGIN SELECT RAISE(ABORT,'injected candidate failure'); END`); err != nil {
		t.Fatal(err)
	}
	terminal := ResponseRunTerminal{ResponseID: l.ResponseID, OwnerInstanceID: "owner", FencingToken: l.FencingToken, Outcome: ResponseRunCompleted, FinalRev: 2, DurableOutputCount: 1}
	if _, err := s.FinalizeResponseRun(ctx, terminal); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("candidate error swallowed: %v", err)
	}
	got, _ := s.GetAgentRun(ctx, a.ID)
	if !got.NotifiedAt.IsZero() {
		t.Fatal("ACK escaped failed finalization")
	}
	var state string
	_ = s.db.QueryRow(`SELECT state FROM serve_response_lifecycle WHERE response_id='wake'`).Scan(&state)
	if state != "running" {
		t.Fatalf("terminal escaped rollback: %s", state)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_notification`); err != nil {
		t.Fatal(err)
	}
	first, err := s.FinalizeResponseRun(ctx, terminal)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.FinalizeResponseRun(ctx, terminal)
	if err != nil || again.LatestAttentionSeq != first.LatestAttentionSeq || notificationCount(t, s, "pending") != 1 {
		t.Fatalf("idempotency failed: %+v %v", again, err)
	}
}

func TestNotificationCommittedSteeringTransfersOnlyHumans(t *testing.T) {
	for _, mode := range []string{"queued", "withdrawn", "synthetic", "human", "human-empty", "human-fenced-out", "rush"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, sid := newAttentionTestStore(t)
			o := notificationTestOrigin(t, s, ctx, "owner")
			other := notificationTestOrigin(t, s, ctx, "other")
			l := admitNotificationTestRun(t, s, ctx, sid, "human", &NotificationTurnAdmission{Human: true, Origin: o})
			origin := &other
			if mode == "human-empty" {
				origin = nil
			}
			msg := core.UserText("steering")
			msg.ClientMessageID = "steer"
			entry := PendingSteering{SessionID: sid, ID: "steer", Message: msg, DisplayText: "steering", Origin: engine.SteeringOriginUser, NotificationOrigin: origin}
			if mode == "synthetic" {
				entry.Origin = engine.SteeringOriginJobNotification
			}
			if err := s.SavePendingSteering(ctx, entry); err != nil {
				t.Fatal(err)
			}
			pending, err := s.ListPendingSteering(ctx, sid)
			if err != nil || len(pending) != 1 {
				t.Fatal(err)
			}
			if mode == "withdrawn" {
				if err := s.DeletePendingSteering(ctx, sid, "steer"); err != nil {
					t.Fatal(err)
				}
			}
			fence := ResponseRunFence{ResponseID: l.ResponseID, OwnerInstanceID: "owner", FencingToken: l.FencingToken}
			if mode == "human-fenced-out" {
				fence.FencingToken++
			}
			if mode == "rush" {
				op, err := s.AdmitRush(ContextWithNotificationOrigin(ctx, other), RushOperation{SessionID: sid, RequestID: "rush", SourceResponseID: l.ResponseID, SourceEpoch: 7, Fence: 1, ReplacementResponseID: l.ResponseID}, pending)
				if err != nil {
					t.Fatal(err)
				}
				op, err = s.AdvanceRush(ctx, op, RushStarting, "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.CommitRushInitialInput(WithResponseRunFence(ctx, fence), op, []*Message{NewMessage(sid, msg, -1)}); err != nil {
					t.Fatal(err)
				}
			} else if mode != "queued" && mode != "withdrawn" {
				_, err := s.AppendMessagesWithTranscriptRev(WithResponseRunFence(ctx, fence), sid, []*Message{NewMessage(sid, msg, -1)})
				if mode == "human-fenced-out" {
					if !errors.Is(err, ErrResponseRunLeaseLost) {
						t.Fatalf("stale fence=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
			var subscription string
			err = s.db.QueryRow(`SELECT subscription_id FROM completion_push_outbox WHERE status='pending'`).Scan(&subscription)
			if mode == "human-empty" {
				if notificationCount(t, s, "pending") != 0 {
					t.Fatal("empty steering kept old target")
				}
				return
			}
			expected := o.SubscriptionID
			if mode == "human" || mode == "rush" {
				expected = other.SubscriptionID
			}
			if err != nil || subscription != expected {
				t.Fatalf("target=%s want=%s error=%v", subscription, expected, err)
			}
		})
	}
}

func TestNotificationCancelStopsHandoffAndNeverNewerTurn(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	o := notificationTestOrigin(t, s, ctx, "owner")
	l := admitNotificationTestRun(t, s, ctx, sid, "old", &NotificationTurnAdmission{Human: true, Origin: o})
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, func(v *ResponseRunTerminal) { v.PendingClientCalls = true; v.PendingClientCallIDs = []string{"call"} })
	if err := s.CancelNotificationTurn(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	l = admitNotificationTestRun(t, s, ctx, sid, "new", &NotificationTurnAdmission{Human: true, Origin: o})
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	if err := s.CancelNotificationTurn(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, s, "pending") != 1 {
		t.Fatal("old Stop canceled newer human")
	}
	if err := s.CancelNotificationTurn(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, s, "pending") != 0 {
		t.Fatal("settled cancellation failed")
	}
}

func TestNotificationQueuedSteeringCapturesOriginBeforePresenceExpires(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	first := notificationTestOrigin(t, s, ctx, "first")
	next := notificationTestOrigin(t, s, ctx, "next")
	l := admitNotificationTestRun(t, s, ctx, sid, "running", &NotificationTurnAdmission{Human: true, Origin: first})
	msg := core.UserText("incorporated later")
	msg.ClientMessageID = "late-steer"
	e := PendingSteering{SessionID: sid, ID: msg.ClientMessageID, Message: msg, Origin: engine.SteeringOriginUser, NotificationOrigin: &next}
	if err := s.SavePendingSteering(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE notification_presence SET expires_at=1 WHERE window_id=?`, next.WindowID); err != nil {
		t.Fatal(err)
	}
	// Durable enqueue retries must not revalidate or erase an accepted origin.
	if err := s.SavePendingSteering(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessagesWithTranscriptRev(WithResponseRunFence(ctx, ResponseRunFence{ResponseID: l.ResponseID, OwnerInstanceID: "owner", FencingToken: l.FencingToken}), sid, []*Message{NewMessage(sid, msg, -1)}); err != nil {
		t.Fatal(err)
	}
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	var sub string
	if err := s.db.QueryRow(`SELECT subscription_id FROM completion_push_outbox WHERE status='pending'`).Scan(&sub); err != nil || sub != next.SubscriptionID {
		t.Fatalf("captured target expired/resampled: %s %v", sub, err)
	}
}

func TestNotificationCancellationSilentAndPermanentFailureOnce(t *testing.T) {
	for _, outcome := range []ResponseRunState{ResponseRunCancelled, ResponseRunFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			s, ctx, sid := newAttentionTestStore(t)
			o := notificationTestOrigin(t, s, ctx, "owner")
			l := admitNotificationTestRun(t, s, ctx, sid, "response", &NotificationTurnAdmission{Human: true, Origin: o})
			terminalNotificationTestRun(t, s, ctx, l, outcome, nil)
			terminalNotificationTestRun(t, s, ctx, l, outcome, nil)
			want := 0
			if outcome == ResponseRunFailed {
				want = 1
			}
			if count := notificationCount(t, s, "pending"); count != want {
				t.Fatalf("outcome=%s notifications=%d", outcome, count)
			}
		})
	}
}

func TestNotificationUnownedPredecessorAndNilTurnStaySilent(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	l := admitNotificationTestRun(t, s, ctx, sid, "custom", &NotificationTurnAdmission{PreviousResponseID: "legacy-unowned", ClientResultIDs: []string{"call"}})
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	l = admitNotificationTestRun(t, s, ctx, sid, "internal", nil)
	terminalNotificationTestRun(t, s, ctx, l, ResponseRunCompleted, nil)
	if notificationCount(t, s, "pending") != 0 {
		t.Fatal("unknown predecessor acquired authority")
	}
}
