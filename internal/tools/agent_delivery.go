package tools

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

// PendingAgentEvents returns trusted, uncollected lifecycle events for a web
// parent. Running rows only become interrupted if their owner is provably dead;
// an unknown or live owner must never be mistaken for a finished child.
func PendingAgentEvents(ctx context.Context, store session.AgentRunDeliveryStore, parent string) ([]session.AgentRun, error) {
	if store == nil || parent == "" {
		return nil, nil
	}
	rows, err := store.ListPendingAgentRuns(ctx)
	if err != nil {
		return nil, err
	}
	var pending []session.AgentRun
	for _, record := range rows {
		if record.ParentSessionID != parent || record.NotifyOrigin != QueueAgentOriginWeb || record.StopReason == "parent_stopped" {
			continue
		}
		if !agentTerminal(record.Status) {
			if record.OwnerInstanceID == processAgentOwner() {
				if _, live := processAgentEntries.Load(record.ID); live {
					continue
				}
			} else if !ownerTerminated(record.OwnerInstanceID) {
				continue
			}
			record.Status = "interrupted"
			record.StopReason = "host_restarted"
		}
		if record.NotifyWhenDone || record.Status == "interrupted" {
			pending = append(pending, record)
		}
	}
	return pending, nil
}

// AcknowledgeAgentEvents is called only after parent execution settles. A
// concurrent wait_agent collection or new child execution invalidates the
// generation check, so it cannot steal an already collected result.
func AcknowledgeAgentEvents(ctx context.Context, store session.AgentRunDeliveryStore, records []session.AgentRun) error {
	for _, record := range records {
		if _, err := store.MarkAgentRunNotified(ctx, record.ID, record.RunGeneration, time.Now()); err != nil {
			return err
		}
	}
	return nil
}
