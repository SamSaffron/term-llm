// Package agentwait carries exact, generation-scoped child wait evidence for host turn finalization.
package agentwait

import (
	"context"

	"github.com/samsaffron/term-llm/internal/session"
)

type agentWaitObserverKey struct{}

// WithObserver reports exact child generations that an explicit
// positive wait budget could not collect. This is continuation evidence, not
// permission to notify: detached spawns and notify_when_done never call it.
func WithObserver(ctx context.Context, observer func(session.AgentRun)) context.Context {
	return context.WithValue(ctx, agentWaitObserverKey{}, observer)
}

func Observe(ctx context.Context, record session.AgentRun, waited bool) {
	if !waited || record.ID == "" || terminal(record.Status) || ctx.Err() != nil {
		return
	}
	if observer, ok := ctx.Value(agentWaitObserverKey{}).(func(session.AgentRun)); ok && observer != nil {
		observer(record)
	}
}

func terminal(status string) bool {
	switch status {
	case "completed", "turn_limit", "cancelled", "interrupted", "failed":
		return true
	}
	return false
}
