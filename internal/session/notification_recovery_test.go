package session

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
)

func TestNotificationHostFailureAfterIncorporatedHumanSteering(t *testing.T) {
	s, ctx, sid := newAttentionTestStore(t)
	original := notificationTestOrigin(t, s, ctx, "original")
	human := notificationTestOrigin(t, s, ctx, "steering")
	lease := admitNotificationTestRun(t, s, ctx, sid, "source", &NotificationTurnAdmission{Human: true, Origin: original})
	child := putNotificationChild(t, s, ctx, sid, "source", "child", 4, "running")
	terminalNotificationTestRun(t, s, ctx, lease, ResponseRunCompleted, func(v *ResponseRunTerminal) {
		v.AwaitingAgentJoin, v.AwaitedEvents = true, []AgentRun{child}
	})
	child = putNotificationChild(t, s, ctx, sid, "source", child.ID, child.RunGeneration, "completed")
	lease = admitNotificationTestRun(t, s, ctx, sid, "wake", &NotificationTurnAdmission{AwaitedEvents: []AgentRun{child}}, child)
	var oldTurn string
	if err := s.db.QueryRow(`SELECT turn_id FROM notification_response_turns WHERE response_id='wake'`).Scan(&oldTurn); err != nil {
		t.Fatal(err)
	}
	msg := core.UserText("change the task during synthesis")
	msg.ClientMessageID = "steering"
	if err := s.SavePendingSteering(ctx, PendingSteering{SessionID: sid, ID: msg.ClientMessageID, Message: msg, Origin: engine.SteeringOriginUser, NotificationOrigin: &human}); err != nil {
		t.Fatal(err)
	}
	fence := ResponseRunFence{ResponseID: lease.ResponseID, OwnerInstanceID: "owner", FencingToken: lease.FencingToken}
	if _, err := s.AppendMessagesWithTranscriptRev(WithResponseRunFence(ctx, fence), sid, []*Message{NewMessage(sid, msg, -1)}); err != nil {
		t.Fatal(err)
	}
	var newTurn, state string
	var awaited int
	if err := s.db.QueryRow(`SELECT n.turn_id,n.state,(SELECT COUNT(*) FROM notification_awaited_agents w WHERE w.turn_id=n.turn_id) FROM notification_turns n JOIN notification_response_turns r ON r.turn_id=n.turn_id WHERE r.response_id='wake'`).Scan(&newTurn, &state, &awaited); err != nil {
		t.Fatal(err)
	}
	if newTurn == oldTurn || state != "running" || awaited != 0 {
		t.Fatalf("steering did not establish fresh human turn: old=%s new=%s state=%s awaited=%d", oldTurn, newTurn, state, awaited)
	}
	attention := terminalNotificationTestRun(t, s, ctx, lease, ResponseRunFailed, func(v *ResponseRunTerminal) { v.Recoverable = true })
	if err := s.db.QueryRow(`SELECT state FROM notification_turns WHERE turn_id=?`, newTurn).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "settled" || !attention.Changed || notificationCount(t, s, "pending") != 1 {
		t.Fatalf("human failure stranded: state=%s attention=%+v", state, attention)
	}
	got, err := s.GetAgentRun(ctx, child.ID)
	if err != nil || !got.NotifiedAt.IsZero() {
		t.Fatalf("failed wake ACKed child: %+v %v", got, err)
	}
	lease = admitNotificationTestRun(t, s, ctx, sid, "retry", &NotificationTurnAdmission{PreviousResponseID: "wake", AwaitedEvents: []AgentRun{child}}, child)
	terminalNotificationTestRun(t, s, ctx, lease, ResponseRunCompleted, nil)
	var linked, stranded int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notification_response_turns WHERE response_id='retry'`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notification_turns WHERE state='waiting_join'`).Scan(&stranded); err != nil {
		t.Fatal(err)
	}
	if linked != 0 || stranded != 0 || notificationCount(t, s, "pending") != 1 {
		t.Fatalf("retry inherited superseded authority or stranded human: linked=%d waiting=%d", linked, stranded)
	}
	rows, err := s.ClaimCompletionNotifications(ctx, "test", time.Now().Add(4*time.Second), time.Minute, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("candidates=%+v err=%v", rows, err)
	}
	if n := rows[0]; n.TurnID != newTurn || n.ResponseID != "wake" || n.Outcome != ResponseRunFailed || n.SubscriptionID != human.SubscriptionID || n.WindowID != human.WindowID || n.DocumentID != human.DocumentID || n.Source != human.Source {
		t.Fatalf("human failure lost exact owner: %+v", n)
	}
}

func TestNotificationRecoverableFailureRequiresResumableAwaitedGeneration(t *testing.T) {
	for _, mode := range []string{"valid", "new_generation", "wrong_parent", "wrong_session", "acked", "collected", "suppressed", "parent_stopped", "stale_response"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, sid := newAttentionTestStore(t)
			origin := notificationTestOrigin(t, s, ctx, "owner")
			lease := admitNotificationTestRun(t, s, ctx, sid, "source", &NotificationTurnAdmission{Human: true, Origin: origin})
			child := putNotificationChild(t, s, ctx, sid, "source", "child", 2, "running")
			terminalNotificationTestRun(t, s, ctx, lease, ResponseRunCompleted, func(v *ResponseRunTerminal) { v.AwaitingAgentJoin, v.AwaitedEvents = true, []AgentRun{child} })
			child = putNotificationChild(t, s, ctx, sid, "source", child.ID, child.RunGeneration, "completed")
			lease = admitNotificationTestRun(t, s, ctx, sid, "wake", &NotificationTurnAdmission{AwaitedEvents: []AgentRun{child}}, child)
			mutations := map[string]string{
				"new_generation": `UPDATE session_agent_runs SET run_generation=3`,
				"wrong_parent":   `UPDATE session_agent_runs SET parent_response_id='unrelated'`,
				"wrong_session":  `UPDATE session_agent_runs SET parent_session_id='unrelated'`,
				"acked":          `UPDATE session_agent_runs SET notified_at=CURRENT_TIMESTAMP`,
				"collected":      `UPDATE session_agent_runs SET collected_at=CURRENT_TIMESTAMP`,
				"suppressed":     `UPDATE session_agent_runs SET wake_suppressed=1`,
				"parent_stopped": `UPDATE session_agent_runs SET stop_reason='parent_stopped'`,
				"stale_response": `UPDATE notification_awaited_agents SET response_id='source'`,
			}
			if query := mutations[mode]; query != "" {
				if _, err := s.db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			// Actual host wakes have no per-run AwaitedEvents on failure.
			attention := terminalNotificationTestRun(t, s, ctx, lease, ResponseRunFailed, func(v *ResponseRunTerminal) { v.Recoverable = true })
			deferred := mode == "valid"
			wantCount := 1
			if deferred {
				wantCount = 0
			}
			var state string
			if err := s.db.QueryRow(`SELECT state FROM notification_turns WHERE current_response_id='wake'`).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if (state == "waiting_join") != deferred || attention.Changed == deferred || notificationCount(t, s, "pending") != wantCount {
				t.Fatalf("mode=%s state=%s attention=%+v", mode, state, attention)
			}
			if deferred {
				got, err := s.GetAgentRun(ctx, child.ID)
				if err != nil || !got.NotifiedAt.IsZero() {
					t.Fatalf("recoverable host failure ACKed child: %+v %v", got, err)
				}
				var turnID string
				if err := s.db.QueryRow(`SELECT turn_id FROM notification_response_turns WHERE response_id='source'`).Scan(&turnID); err != nil {
					t.Fatal(err)
				}
				lease = admitNotificationTestRun(t, s, ctx, sid, "retry", &NotificationTurnAdmission{AwaitedEvents: []AgentRun{child}}, child)
				attention = terminalNotificationTestRun(t, s, ctx, lease, ResponseRunCompleted, nil)
				got, err = s.GetAgentRun(ctx, child.ID)
				if err != nil || got.NotifiedAt.IsZero() || !attention.Changed {
					t.Fatalf("host retry did not settle and ACK: %+v %+v %v", attention, got, err)
				}
				rows, err := s.ClaimCompletionNotifications(ctx, "test", time.Now().Add(4*time.Second), time.Minute, 10)
				if err != nil || len(rows) != 1 {
					t.Fatalf("retry candidates=%+v err=%v", rows, err)
				}
				if n := rows[0]; n.TurnID != turnID || n.ResponseID != "retry" || n.Outcome != ResponseRunCompleted || n.SubscriptionID != origin.SubscriptionID || n.WindowID != origin.WindowID || n.DocumentID != origin.DocumentID || n.Source != origin.Source {
					t.Fatalf("host retry lost original turn/owner: %+v", n)
				}
			}
		})
	}
}

func TestNotificationRecoverableHostFailurePreservesUnfinishedSibling(t *testing.T) {
	for _, status := range []string{"queued", "running", "awaiting_approval"} {
		t.Run(status, func(t *testing.T) {
			s, ctx, sid := newAttentionTestStore(t)
			origin := notificationTestOrigin(t, s, ctx, "owner")
			lease := admitNotificationTestRun(t, s, ctx, sid, "source", &NotificationTurnAdmission{Human: true, Origin: origin})
			first := putNotificationChild(t, s, ctx, sid, "source", "first", 2, "running")
			sibling := putNotificationChild(t, s, ctx, sid, "source", "sibling", 3, status)
			terminalNotificationTestRun(t, s, ctx, lease, ResponseRunCompleted, func(v *ResponseRunTerminal) {
				v.AwaitingAgentJoin, v.AwaitedEvents = true, []AgentRun{first, sibling}
			})
			var turnID string
			if err := s.db.QueryRow(`SELECT turn_id FROM notification_response_turns WHERE response_id='source'`).Scan(&turnID); err != nil {
				t.Fatal(err)
			}
			first = putNotificationChild(t, s, ctx, sid, "source", first.ID, first.RunGeneration, "completed")
			lease = admitNotificationTestRun(t, s, ctx, sid, "eager-wake", &NotificationTurnAdmission{AwaitedEvents: []AgentRun{first}}, first)
			// Collection during synthesis retires the only currently ready event,
			// but the exact unfinished sibling can still resume this turn later.
			if err := s.CollectAgentRunGeneration(ctx, first.ID, first.RunGeneration, time.Now()); err != nil {
				t.Fatal(err)
			}
			attention := terminalNotificationTestRun(t, s, ctx, lease, ResponseRunFailed, func(v *ResponseRunTerminal) { v.Recoverable = true })
			var state string
			if err := s.db.QueryRow(`SELECT state FROM notification_turns WHERE turn_id=?`, turnID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "waiting_join" || attention.Changed || notificationCount(t, s, "pending") != 0 {
				t.Fatalf("unfinished sibling finalized early: state=%s attention=%+v", state, attention)
			}
			for _, event := range []AgentRun{first, sibling} {
				got, err := s.GetAgentRun(ctx, event.ID)
				if err != nil || !got.NotifiedAt.IsZero() {
					t.Fatalf("failed eager wake ACKed child: %+v %v", got, err)
				}
			}
			sibling = putNotificationChild(t, s, ctx, sid, "source", sibling.ID, sibling.RunGeneration, "completed")
			lease = admitNotificationTestRun(t, s, ctx, sid, "sibling-wake", &NotificationTurnAdmission{AwaitedEvents: []AgentRun{sibling}}, sibling)
			attention = terminalNotificationTestRun(t, s, ctx, lease, ResponseRunCompleted, nil)
			got, err := s.GetAgentRun(ctx, sibling.ID)
			if err != nil || got.NotifiedAt.IsZero() || !attention.Changed {
				t.Fatalf("sibling final did not settle/ACK: %+v %+v %v", attention, got, err)
			}
			rows, err := s.ClaimCompletionNotifications(ctx, "test", time.Now().Add(4*time.Second), time.Minute, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("sibling final candidates=%+v err=%v", rows, err)
			}
			if n := rows[0]; n.TurnID != turnID || n.ResponseID != "sibling-wake" || n.SubscriptionID != origin.SubscriptionID || n.WindowID != origin.WindowID || n.DocumentID != origin.DocumentID || n.Source != origin.Source {
				t.Fatalf("unfinished sibling lost exact original turn/owner: %+v", n)
			}
		})
	}
}
