package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// explicitParentStop distinguishes a human cancellation from a host reload,
// response inactivity timeout, or lease loss. Only the former suppresses
// automatic parent recovery; none of them automatically resume children.
func explicitParentStop(ctx context.Context) bool {
	if errors.Is(context.Cause(ctx), restart.ErrInterrupt) {
		return false
	}
	if run := responseRunFromContext(ctx); run != nil {
		run.mu.Lock()
		stopped := run.cancelRequested
		run.mu.Unlock()
		return stopped
	}
	return true // interactive/non-response hosts treat a cancelled turn as a stop
}

func (rt *serveRuntime) signalStoppedChildren(ctx context.Context, parent string) func(context.Context) []string {
	if !explicitParentStop(ctx) {
		if errors.Is(context.Cause(ctx), restart.ErrInterrupt) {
			return tools.InterruptAgentsForParentAsync(parent)
		}
		// A response inactivity timeout/lease loss is not a child deadline.
		// The detached children remain independently owned by the host.
		return nil
	}
	if delivery := session.AsAgentRunDeliveryStore(rt.store); delivery != nil && parent != "" {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		var err error
		if run := responseRunFromContext(ctx); run != nil {
			err = delivery.SuppressAgentWakesForTurn(persistCtx, parent, run.id)
			if err == nil {
				err = delivery.SuppressAgentEvents(persistCtx, run.agentEvents)
			}
		} else {
			err = delivery.SuppressPendingAgentWakes(persistCtx, parent)
		}
		if err != nil {
			runtimeoutput.Logf("suppress stopped parent wake %s: %v", parent, err)
		}
		cancel()
	}
	return tools.StopAgentsForParent(parent)
}
