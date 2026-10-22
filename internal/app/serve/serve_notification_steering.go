package serve

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

func (rt *serveRuntime) configurePendingSteeringPersistence(state *runtimeInterruptState, sessionID string) {
	pendingStore, ok := session.AsPendingSteeringStore(rt.store)
	if state == nil || !ok || strings.TrimSpace(sessionID) == "" {
		return
	}
	state.persistPendingSteering = func(persistCtx context.Context, entry engine.QueuedSteering) error {
		if strings.TrimSpace(entry.ID) == "" {
			return nil
		}
		displayText := strings.TrimSpace(entry.DisplayText)
		if displayText == "" {
			displayText = strings.TrimSpace(core.MessageText(entry.Message))
		}
		attachmentSummary := strings.TrimSpace(core.MessageAttachmentSummary(entry.Message))
		if persistCtx == nil {
			persistCtx = context.Background()
		} else {
			persistCtx = context.WithoutCancel(persistCtx)
		}
		dbCtx, cancel := inlinePersistContext(persistCtx, 4*time.Second)
		defer cancel()
		return pendingStore.SavePendingSteering(dbCtx, session.PendingSteering{
			NotificationOrigin: notificationSteeringOrigin(persistCtx, entry),
			SessionID:          sessionID,
			ID:                 entry.ID,
			Origin:             entry.Origin,
			Message:            entry.Message,
			DisplayText:        displayText,
			AttachmentSummary:  attachmentSummary,
			CreatedAt:          time.Now(),
		})
	}
	state.removePendingSteering = func(removeCtx context.Context, id string) {
		dbCtx, cancel := inlinePersistContext(removeCtx, 10*time.Second)
		defer cancel()
		if err := pendingStore.DeletePendingSteering(dbCtx, sessionID, id); err != nil {
			log.Printf("[serve] delete pending steering %s/%s failed: %v", sessionID, id, err)
		}
	}
}

func notificationSteeringOrigin(ctx context.Context, entry engine.QueuedSteering) *session.NotificationOrigin {
	if entry.Origin != engine.SteeringOriginUser {
		return nil
	}
	origin, _ := session.NotificationOriginFromContext(ctx)
	return &origin
}
