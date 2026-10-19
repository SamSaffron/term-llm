package agentbarrier

import (
	"context"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// NeedsResources fails closed when child state cannot be reconciled.
func NeedsResources(ctx context.Context, store session.AgentRunDeliveryStore, parent string) bool {
	// Reconciliation also retries failed local terminal persistence before a
	// terminal row can be mistaken for an empty/fully observed durable registry.
	if events, err := tools.PendingAgentBarrier(ctx, store, parent); err != nil || len(events) > 0 {
		return true
	}
	rows, err := store.ListAgentRuns(ctx, parent)
	if err != nil {
		return true
	}
	for _, row := range rows {
		switch row.Status {
		case "completed", "turn_limit", "cancelled", "interrupted", "failed":
		default:
			return true
		}
		if row.NotifiedAt.IsZero() && row.CollectedAt.IsZero() && !row.WakeSuppressed && row.StopReason != "parent_stopped" {
			return true
		}
	}
	return false
}
