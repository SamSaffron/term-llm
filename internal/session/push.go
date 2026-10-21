package session

import (
	"context"
	"time"
)

// Push subscription client kinds distinguish browser enrollment from native apps.
const (
	PushClientBrowser = "browser"
	PushClientApp     = "app"
)

// PushSubscription represents a Web Push subscription stored in the database.
type PushSubscription struct {
	ID              string
	Endpoint        string
	ClientKind      string // PushClientBrowser or PushClientApp; blank inputs default to browser.
	KeyP256DH       string
	KeyAuth         string
	Status          string
	VAPIDKeyID      string
	UpdatedAt       time.Time
	LastUsedAt      time.Time
	LastFailureCode string
	LastFailure     string
	LastFailureAt   time.Time
}

type PushSubscriptionLifecycleStore interface {
	UpsertPushSubscription(ctx context.Context, sub *PushSubscription) (*PushSubscription, error)
	GetPushSubscription(ctx context.Context, id string) (*PushSubscription, error)
	DeletePushSubscriptionByID(ctx context.Context, id string) error
	MarkPushSubscriptionStale(ctx context.Context, id, code, detail string) error
	MarkPushSubscriptionUsed(ctx context.Context, id string) error
}

func AsPushSubscriptionLifecycleStore(store Store) (PushSubscriptionLifecycleStore, bool) {
	if store == nil {
		return nil, false
	}
	if lifecycle, ok := store.(PushSubscriptionLifecycleStore); ok {
		return lifecycle, true
	}
	if logging, ok := store.(*LoggingStore); ok {
		lifecycle, ok := logging.Store.(PushSubscriptionLifecycleStore)
		return lifecycle, ok
	}
	return nil, false
}
