package serve

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools/agentwait"
)

func withNotificationWaitObserver(ctx context.Context, run *responseRun) context.Context {
	return agentwait.WithObserver(ctx, func(child session.AgentRun) {
		if child.ParentSessionID != run.sessionID || child.ParentResponseID == "" {
			return
		}
		run.mu.Lock()
		defer run.mu.Unlock()
		for i, previous := range run.awaitedEvents {
			if previous.ID == child.ID {
				run.awaitedEvents[i] = child
				return
			}
		}
		run.awaitedEvents = append(run.awaitedEvents, child)
	})
}

// Called under run.mu by the existing fenced lifecycle finalizer.
func notificationTerminal(run *responseRun, outcome session.ResponseRunState, finalRev int64, count int, owner string, fence int64) session.ResponseRunTerminal {
	ids := make([]string, 0, len(run.pendingClientCalls))
	for _, call := range run.pendingClientCalls {
		if id := stringValue(call["call_id"]); id != "" {
			ids = append(ids, id)
		}
	}
	return session.ResponseRunTerminal{ResponseID: run.id, OwnerInstanceID: owner, FencingToken: fence, Outcome: outcome, FinalRev: finalRev, DurableOutputCount: count, EndedAt: time.Now().UTC(), PendingClientCalls: len(run.pendingClientCalls) > 0, PendingClientCallIDs: ids, AwaitedEvents: run.awaitedEvents, AwaitingAgentJoin: len(run.awaitedEvents) > 0, Recoverable: run.recoverableNotification, AgentEvents: run.agentEvents}
}
