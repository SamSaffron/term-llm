package session

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session/notificationdb"
)

// PendingSteering is a durable, not-yet-committed steering intent. It lives
// outside the transcript until the engine consumes it, so restoring a session
// cannot accidentally send the same user message to a provider early.
type PendingSteering struct {
	NotificationOrigin *NotificationOrigin   `json:"notification_origin,omitempty"`
	SessionID          string                `json:"session_id"`
	ID                 string                `json:"id"`
	Message            core.Message          `json:"message"`
	DisplayText        string                `json:"display_text"`
	AttachmentSummary  string                `json:"attachment_summary"`
	CreatedAt          time.Time             `json:"created_at"`
	AcceptanceSequence int64                 `json:"acceptance_sequence"`
	Origin             engine.SteeringOrigin `json:"origin"`
	OwnerKind          string                `json:"owner_kind"`
	OwnerID            string                `json:"owner_id"`
	OwnerFence         int64                 `json:"owner_fence"`
}

// NotificationOrigin identifies one authenticated, capability-negotiated window.
// An empty origin still supersedes older human turns but grants no OS authority.
type NotificationOrigin = notificationdb.NotificationOrigin

type NotificationTurnAdmission struct {
	ClientResultIDs    []string
	Human              bool
	PreviousResponseID string
	Origin             NotificationOrigin
	AwaitedEvents      []AgentRun
}

type notificationOriginContextKey struct{}

func ContextWithNotificationOrigin(ctx context.Context, origin NotificationOrigin) context.Context {
	return context.WithValue(ctx, notificationOriginContextKey{}, origin)
}
func NotificationOriginFromContext(ctx context.Context) (NotificationOrigin, bool) {
	if ctx == nil {
		return NotificationOrigin{}, false
	}
	origin, ok := ctx.Value(notificationOriginContextKey{}).(NotificationOrigin)
	return origin, ok
}

var ErrNotificationLineage = notificationdb.ErrNotificationLineage
var ErrNotificationPresenceConflict = notificationdb.ErrNotificationPresenceConflict
var ErrNotificationClaimLost = notificationdb.ErrNotificationClaimLost

type NotificationPresence = notificationdb.NotificationPresence

type CompletionNotification = notificationdb.CompletionNotification

type NotificationDeliveryStore interface {
	UpdateNotificationPresence(context.Context, NotificationPresence) error
	ClaimCompletionNotifications(context.Context, string, time.Time, time.Duration, int) ([]CompletionNotification, error)
	RecheckCompletionNotification(context.Context, int64, string, string, time.Time) (bool, error)
	CompleteCompletionNotification(context.Context, int64, string, string, string, time.Time, string) error
}

func AsNotificationDeliveryStore(store Store) (NotificationDeliveryStore, bool) {
	if logging, ok := store.(*LoggingStore); ok {
		return AsNotificationDeliveryStore(logging.Store)
	}
	capability, ok := store.(NotificationDeliveryStore)
	return capability, ok
}

type NotificationTurnStore interface {
	CancelNotificationTurn(context.Context, string) error
	SupersedeNotificationTurn(context.Context, ResponseRunFence, NotificationOrigin) error
}

func AsNotificationTurnStore(store Store) (NotificationTurnStore, bool) {
	if logging, ok := store.(*LoggingStore); ok {
		return AsNotificationTurnStore(logging.Store)
	}
	capability, ok := store.(NotificationTurnStore)
	return capability, ok
}
