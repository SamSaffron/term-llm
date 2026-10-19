// Package agentbarrier owns persisted TUI lifecycle delivery settlement.
package agentbarrier

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const attemptTimeout = 4 * time.Second

// Complete retries persistence, never provider execution. ACK requires an owned
// lease; human suppression is generation-scoped and may outlive lease loss.
func Complete(ctx, settlementCtx, shutdownCtx context.Context, store session.AgentRunDeliveryStore, explicitStop *atomic.Bool, events []session.AgentRun, runErr error) error {
	delay := 50 * time.Millisecond
	for {
		stopped := explicitStop.Load()
		stopCtx := shutdownCtx
		attemptParent := ctx
		if stopped {
			if err := stopCtx.Err(); err != nil {
				return err
			}
			attemptParent = stopCtx
		} else {
			if err := settlementCtx.Err(); err != nil {
				return err
			}
			if runErr != nil {
				return runErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(events) == 0 {
				return nil
			}
		}
		attemptCtx, cancel := context.WithTimeout(attemptParent, attemptTimeout)
		var err error
		if stopped {
			if store != nil {
				err = suppress(attemptCtx, store, core.SessionIDFromContext(ctx), parentResponseID(ctx), events)
			}
		} else {
			// The store acknowledges exact generations. Already collected,
			// suppressed or superseded events are successful no-ops, not retries
			// of provider execution; newly offered generations remain pending.
			err = tools.AcknowledgeAgentEvents(attemptCtx, store, events)
		}
		cancel()
		if err == nil {
			if stopped {
				return context.Canceled
			}
			if !explicitStop.Load() {
				return nil
			}
			continue // Stop raced the ACK: preserve its suppression intent.
		}
		// Only settlement is retried. There is no retry budget which could
		// release ownership and repeat inference/side effects after a good turn.
		waitCtx, ownerCtx := ctx, settlementCtx
		if explicitStop.Load() {
			waitCtx, ownerCtx = stopCtx, stopCtx
		}
		timer := time.NewTimer(delay)
		select {
		case <-ownerCtx.Done():
			timer.Stop()
			if !explicitStop.Load() {
				return ownerCtx.Err()
			}
		case <-waitCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
		delay = min(delay*2, time.Second)
	}
}

func suppress(ctx context.Context, store session.AgentRunDeliveryStore, parent, response string, offered []session.AgentRun) error {
	rows, err := store.ListAgentRuns(ctx, parent)
	if err != nil {
		return err
	}
	events := append([]session.AgentRun(nil), offered...)
	for _, row := range rows {
		// Do not sweep currently-running children: a successor may own them after
		// lease loss. Runtime cancellation handles children running at human Stop.
		if response != "" && row.ParentResponseID == response {
			events = append(events, row)
		}
	}
	return store.SuppressAgentEvents(ctx, events)
}

func parentResponseID(ctx context.Context) string {
	fence, _ := session.ResponseRunFenceFromContext(ctx)
	return fence.ResponseID
}

// PersistNotice writes synthetic input only after run admission.
func PersistNotice(ctx context.Context, store session.Store, events []session.AgentRun, messages []core.Message, boundary *runboundary.Tracker) ([]core.Message, *session.Message, error) {
	if err := CheckRun(ctx); err != nil {
		return nil, nil, err
	}
	notice := tools.AgentLifecycleMessage(events, true)
	if len(messages) > 0 && sameNotice(messages[len(messages)-1], notice) {
		rows, err := store.GetMessages(ctx, core.SessionIDFromContext(ctx), 0, 0)
		if err != nil {
			return nil, nil, err
		}
		if len(rows) > 0 && rows[len(rows)-1].IsGoalSteering() && sameNotice(rows[len(rows)-1].ToLLMMessage(), notice) {
			// An unanswered delivery attempt already persisted this exact set of
			// generations. Reuse its anchor without another row or UI notice.
			messages = append([]core.Message(nil), messages...)
			messages[len(messages)-1] = notice
			snapshot := boundary.CompletedSnapshot()
			boundary.Reset(boundary.RunID(), messages, rows[len(rows)-1].ID, true, snapshot.Provider)
			return messages, nil, nil
		}
	}
	row := session.NewMessage(core.SessionIDFromContext(ctx), notice, -1)
	if err := store.AddMessage(ctx, row.SessionID, row); err != nil {
		return nil, nil, err
	}
	messages = append(messages, notice)
	snapshot := boundary.CompletedSnapshot()
	boundary.Reset(boundary.RunID(), messages, row.ID, true, snapshot.Provider)
	return messages, row, nil
}

func sameNotice(message, notice core.Message) bool {
	return message.Role == core.RoleUser && core.MessageText(message) == core.MessageText(notice)
}
