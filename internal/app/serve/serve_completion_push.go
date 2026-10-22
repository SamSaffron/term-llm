package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/notification"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

type completionPushPayload = notification.Envelope

func (s *serveServer) validateCompletionPushTarget(ctx context.Context, id string) (string, error) {
	store, ok := session.AsPushSubscriptionLifecycleStore(s.store)
	if !ok || s.cfgRef == nil || !s.cfgRef.Serve.WebPush.PublicKeyRef().Configured() {
		return "", fmt.Errorf("completion notifications are unavailable")
	}
	sub, err := store.GetPushSubscription(ctx, strings.TrimSpace(id))
	if err != nil {
		return "", fmt.Errorf("verify completion notification subscription: %w", err)
	}
	if sub == nil || sub.Status != "active" {
		return "", fmt.Errorf("completion notification subscription is stale or missing")
	}
	publicKey, err := appnotify.WebPushPublicKey(s.cfgRef)
	if err != nil {
		return "", fmt.Errorf("completion notifications are temporarily unavailable: %w", err)
	}
	if publicKey == "" {
		return "", fmt.Errorf("completion notifications are unavailable")
	}
	if sub.VAPIDKeyID != appnotify.WebPushKeyID(publicKey) {
		_ = store.MarkPushSubscriptionStale(ctx, sub.ID, "vapid_rotated", "server push key changed")
		return "", fmt.Errorf("completion notification subscription uses an old server key")
	}
	return sub.ID, nil
}

// completionPushLabel returns a short label identifying the node that produced
// a completion notification, or "" when this deployment has no node identity
// configured.
//
// A Hub fronts several nodes and every node sends byte-identical notification
// text, so once more than one node has a push subscription the notifications
// are indistinguishable until you open one. The payload already carries a
// node-specific URL, but the visible title does not.
//
// agentName is deliberately not consulted: nearly every deployment passes
// --agent, and single-node users should keep the original wording.
func (c serveServerConfig) completionPushLabel() string {
	for _, candidate := range []string{c.uiTitle, c.hubNodeName, c.hubNodeID} {
		if label := strings.TrimSpace(candidate); label != "" {
			return label
		}
	}
	return ""
}

func (s *serveServer) completionNotificationPayload(item session.CompletionNotification) ([]byte, error) {
	title, body := "Response complete", "Your term-llm response is ready."
	if item.Outcome == session.ResponseRunFailed {
		title, body = "Response failed", "Your term-llm response stopped with an error."
	}
	if label := s.cfg.completionPushLabel(); label != "" {
		title = label + ": " + title
	}
	return json.Marshal(completionPushPayload{
		Version: notification.Version, Kind: notification.KindCompletion, EventID: item.EventID,
		TurnID: item.TurnID, ResponseID: item.ResponseID, WindowID: item.WindowID, Source: item.Source,
		StoreInstanceID: item.StoreInstanceID, SessionID: item.SessionID, AttentionSeq: item.AttentionSeq,
		RunEpoch: item.RunEpoch, Outcome: string(item.Outcome), Title: title, Body: body,
		URL:       strings.TrimSuffix(item.Source, "/") + "/chat/" + url.PathEscape(item.SessionID),
		CreatedAt: item.CreatedAt.UTC().Format(time.RFC3339Nano), ExpiresAt: item.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *serveServer) startCompletionPushDispatcher() {
	if _, ok := session.AsNotificationDeliveryStore(s.store); !ok || !appnotify.WebPushConfigured(s.cfgRef) {
		return
	}
	if s.completionPushWake == nil {
		s.completionPushWake = make(chan struct{}, 1)
	}
	s.completionPushWG.Add(1)
	go func() {
		defer s.completionPushWG.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			s.dispatchCompletionPushes(nil)
			select {
			case <-s.shutdownCh:
				return
			case <-s.completionPushWake:
			case <-ticker.C:
			}
		}
	}()
}

// The legacy parameter is retained for internal test callers; only the fenced
// durable logical-turn capability can authorize a send. Legacy outbox rows are
// retired by the schema migration, never promoted by this dispatcher.
func (s *serveServer) dispatchCompletionPushes(_ session.CompletionPushOutboxStore) {
	base, release, err := restart.Default.Activity(context.Background())
	if err != nil {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(base, 20*time.Second)
	defer cancel()
	s.dispatchCompletionPushesWithSender(ctx, nil, 20*time.Second, appnotify.SendWebPushDetailed)
}

func (s *serveServer) dispatchCompletionPushesWithSender(ctx context.Context, _ session.CompletionPushOutboxStore, attemptTimeout time.Duration, send func(context.Context, *session.PushSubscription, []byte, *webpush.Options) (int, time.Duration, error)) {
	outbox, ok := session.AsNotificationDeliveryStore(s.store)
	if !ok {
		return
	}
	pushStore, ok := session.AsPushSubscriptionLifecycleStore(s.store)
	if !ok {
		return
	}
	publicKey, privateKey, keyErr := appnotify.WebPushKeys(s.cfgRef)
	if keyErr != nil {
		log.Printf("warning: completion push deferred: %v", keyErr)
		return
	}
	// Claim longer than the whole dispatch budget so the external send and its
	// bounded result write cannot normally outlive their fence.
	owner := s.responseOwnerID()
	items, err := outbox.ClaimCompletionNotifications(ctx, owner, time.Now().UTC(), 30*time.Second, 25)
	if err != nil {
		log.Printf("warning: claim completion notifications: %v", err)
		return
	}
	for index, item := range items {
		if ctx.Err() != nil {
			for _, pending := range items[index:] {
				s.persistNotificationResult(outbox, pending, "defer", time.Time{}, "")
			}
			break
		}
		sub, getErr := pushStore.GetPushSubscription(ctx, item.SubscriptionID)
		if getErr != nil {
			s.recordNotificationResult(outbox, pushStore, item, nil, 0, 0, getErr)
			continue
		}
		if sub == nil || sub.Status != "active" || sub.VAPIDKeyID != appnotify.WebPushKeyID(publicKey) {
			s.persistNotificationResult(outbox, item, "dead", time.Time{}, "subscription is stale or missing")
			continue
		}
		payload, err := s.completionNotificationPayload(item)
		if err != nil {
			s.persistNotificationResult(outbox, item, "dead", time.Time{}, "invalid notification metadata")
			continue
		}
		ready, err := outbox.RecheckCompletionNotification(ctx, item.ID, item.ClaimOwner, item.ClaimToken, time.Now().UTC())
		if err != nil {
			log.Printf("warning: recheck completion notification %d: %v", item.ID, err)
			continue
		}
		if !ready {
			continue
		}
		ttl := min(300, int(time.Until(item.ExpiresAt).Seconds()))
		if ttl <= 0 {
			s.persistNotificationResult(outbox, item, "dead", time.Time{}, "notification expired")
			continue
		}
		opts := appnotify.NewWebPushOptions(publicKey, privateKey, s.cfgRef.Serve.WebPush.Subject, ttl)
		opts.Topic = notification.Topic(item.EventID)
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		status, retryAfter, sendErr := sendCompletionPush(attemptCtx, sub, payload, opts, send)
		cancel()
		s.recordNotificationResult(outbox, pushStore, item, sub, status, retryAfter, sendErr)
	}
	if legacy, ok := session.AsCompletionPushOutboxStore(s.store); ok {
		if err := legacy.PruneCompletionPushOutbox(ctx, time.Now().UTC().Add(-7*24*time.Hour)); err != nil && ctx.Err() == nil {
			log.Printf("warning: prune completion notifications: %v", err)
		}
	}
}

func (s *serveServer) persistNotificationResult(outbox session.NotificationDeliveryStore, item session.CompletionNotification, status string, next time.Time, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := outbox.CompleteCompletionNotification(ctx, item.ID, item.ClaimOwner, item.ClaimToken, status, next, detail); err != nil {
		log.Printf("warning: persist completion notification %d result (%s): %v", item.ID, status, err)
	}
}

func (s *serveServer) recordNotificationResult(outbox session.NotificationDeliveryStore, pushStore session.PushSubscriptionLifecycleStore, item session.CompletionNotification, sub *session.PushSubscription, status int, retryAfter time.Duration, sendErr error) {
	if sendErr == nil {
		s.persistNotificationResult(outbox, item, "delivered", time.Time{}, "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pushStore.MarkPushSubscriptionUsed(ctx, sub.ID); err != nil {
			log.Printf("warning: persist completion subscription use: %v", err)
		}
		return
	}
	if status == http.StatusGone || status == http.StatusNotFound {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pushStore.MarkPushSubscriptionStale(ctx, sub.ID, fmt.Sprintf("http_%d", status), "push endpoint rejected subscription"); err != nil {
			log.Printf("warning: retire completion subscription: %v", err)
		}
		s.persistNotificationResult(outbox, item, "dead", time.Time{}, "push endpoint rejected the subscription")
		return
	}
	if errors.Is(sendErr, webpush.ErrMaxPadExceeded) || status == http.StatusRequestEntityTooLarge {
		s.persistNotificationResult(outbox, item, "dead", time.Time{}, "push payload too large")
		return
	}
	if item.AttemptCount >= 7 {
		s.persistNotificationResult(outbox, item, "dead", time.Time{}, "delivery retry limit reached")
		return
	}
	delay := retryAfter
	if delay <= 0 {
		delay = time.Second * time.Duration(1<<min(item.AttemptCount, 8))
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	s.persistNotificationResult(outbox, item, "retry", time.Now().UTC().Add(delay), "temporary delivery failure")
}
