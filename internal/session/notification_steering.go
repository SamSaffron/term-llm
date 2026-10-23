package session

import (
	"context"
	"errors"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session/notificationdb"
)

func saveNotificationSteeringOriginTx(ctx context.Context, q sqliteQueryExecer, e PendingSteering) error {
	origin := e.NotificationOrigin
	if origin == nil {
		if value, ok := NotificationOriginFromContext(ctx); ok {
			origin = &value
		}
	}
	err := notificationdb.SaveSteeringOriginTx(ctx, q, e.SessionID, e.ID, origin, e.Origin == engine.SteeringOriginUser)
	if errors.Is(err, notificationdb.ErrSteeringOriginConflict) {
		return ErrSteeringConflict
	}
	return err
}
func incorporateNotificationSteeringTx(ctx context.Context, q sqliteQueryExecer, sessionID string, msg *Message) error {
	if msg.Role != "user" || msg.CompactionTail || msg.ClientMessageID == "" {
		return nil
	}
	fence, _ := ResponseRunFenceFromContext(ctx)
	return notificationdb.IncorporateSteeringTx(ctx, q, sessionID, msg.ClientMessageID, fence.ResponseID)
}
