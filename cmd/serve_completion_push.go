package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/session"
)

const completionPushSchemaVersion = 1

type completionPushPayload struct {
	Version    int    `json:"version"`
	EventID    string `json:"event_id"`
	ResponseID string `json:"response_id"`
	SessionID  string `json:"session_id"`
	Outcome    string `json:"outcome"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	URL        string `json:"url"`
	CreatedAt  string `json:"created_at"`
}

func completionPushEventID(responseID, subscriptionID string) string {
	return "completion:" + responseID + ":" + subscriptionID
}

func (s *serveServer) validateCompletionPushTarget(ctx context.Context, id string) (string, error) {
	store, ok := session.AsPushSubscriptionLifecycleStore(s.store)
	if !ok || s.cfgRef == nil || strings.TrimSpace(s.cfgRef.Serve.WebPush.VAPIDPublicKey) == "" {
		return "", fmt.Errorf("completion notifications are unavailable")
	}
	sub, err := store.GetPushSubscription(ctx, strings.TrimSpace(id))
	if err != nil {
		return "", fmt.Errorf("verify completion notification subscription: %w", err)
	}
	if sub == nil || sub.Status != "active" {
		return "", fmt.Errorf("completion notification subscription is stale or missing")
	}
	if sub.VAPIDKeyID != webPushKeyID(s.cfgRef.Serve.WebPush.VAPIDPublicKey) {
		_ = store.MarkPushSubscriptionStale(ctx, sub.ID, "vapid_rotated", "server push key changed")
		return "", fmt.Errorf("completion notification subscription uses an old server key")
	}
	return sub.ID, nil
}

func (s *serveServer) enqueueCompletionPush(responseID, sessionID, subscriptionID, outcome string, createdAt time.Time) {
	outbox, ok := session.AsCompletionPushOutboxStore(s.store)
	if !ok || strings.TrimSpace(subscriptionID) == "" || (outcome != "completed" && outcome != "failed") {
		return
	}
	eventID := completionPushEventID(responseID, subscriptionID)
	title := "Response complete"
	body := "Your term-llm response is ready."
	if outcome == "failed" {
		title = "Response failed"
		body = "Your term-llm response stopped with an error."
	}
	base := strings.TrimSuffix(s.cfg.basePath, "/")
	if base == "" {
		base = "/ui"
	}
	path := base + "/chat/" + url.PathEscape(sessionID)
	payload, err := json.Marshal(completionPushPayload{
		Version: completionPushSchemaVersion, EventID: eventID, ResponseID: responseID,
		SessionID: sessionID, Outcome: outcome, Title: title, Body: body, URL: path,
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return
	}
	inserted, err := outbox.EnqueueCompletionPush(context.Background(), session.CompletionPushOutboxItem{
		EventID: eventID, ResponseID: responseID, SubscriptionID: subscriptionID, Payload: payload,
	})
	if err == nil && inserted && s.completionPushWake != nil {
		select {
		case s.completionPushWake <- struct{}{}:
		default:
		}
	}
}

func (s *serveServer) startCompletionPushDispatcher() {
	outbox, ok := session.AsCompletionPushOutboxStore(s.store)
	if !ok || s.cfgRef == nil || strings.TrimSpace(s.cfgRef.Serve.WebPush.VAPIDPublicKey) == "" || strings.TrimSpace(s.cfgRef.Serve.WebPush.VAPIDPrivateKey) == "" {
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
			s.dispatchCompletionPushes(outbox)
			select {
			case <-s.shutdownCh:
				return
			case <-s.completionPushWake:
			case <-ticker.C:
			}
		}
	}()
}

func (s *serveServer) dispatchCompletionPushes(outbox session.CompletionPushOutboxStore) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	items, err := outbox.ListDueCompletionPushes(ctx, time.Now().UTC(), 25)
	if err != nil || len(items) == 0 {
		return
	}
	pushStore, ok := session.AsPushSubscriptionLifecycleStore(s.store)
	if !ok {
		return
	}
	opts := &webpush.Options{
		VAPIDPublicKey: s.cfgRef.Serve.WebPush.VAPIDPublicKey, VAPIDPrivateKey: s.cfgRef.Serve.WebPush.VAPIDPrivateKey,
		Subscriber: normalizeWebPushSubject(s.cfgRef.Serve.WebPush.Subject), TTL: 300,
	}
	for _, item := range items {
		sub, getErr := pushStore.GetPushSubscription(ctx, item.SubscriptionID)
		if getErr != nil {
			s.retryCompletionPush(ctx, outbox, item, getErr)
			continue
		}
		if sub == nil || sub.Status != "active" || sub.VAPIDKeyID != webPushKeyID(s.cfgRef.Serve.WebPush.VAPIDPublicKey) {
			_ = outbox.MarkCompletionPushDead(ctx, item.ID, "subscription is stale or missing")
			continue
		}
		status, retryAfter, sendErr := sendWebPushDetailed(ctx, sub, item.Payload, opts)
		if sendErr == nil {
			_ = pushStore.MarkPushSubscriptionUsed(ctx, sub.ID)
			_ = outbox.MarkCompletionPushDelivered(ctx, item.ID)
			continue
		}
		if status == http.StatusGone || status == http.StatusNotFound {
			_ = pushStore.MarkPushSubscriptionStale(ctx, sub.ID, fmt.Sprintf("http_%d", status), "push endpoint rejected the subscription")
			_ = outbox.MarkCompletionPushDead(ctx, item.ID, "push endpoint rejected the subscription")
			continue
		}
		if item.AttemptCount >= 7 {
			_ = outbox.MarkCompletionPushDead(ctx, item.ID, "delivery retry limit reached")
			continue
		}
		s.retryCompletionPushAfter(ctx, outbox, item, sendErr, retryAfter)
	}
	_ = outbox.PruneCompletionPushOutbox(ctx, time.Now().UTC().Add(-7*24*time.Hour))
}

func (s *serveServer) retryCompletionPush(ctx context.Context, outbox session.CompletionPushOutboxStore, item session.CompletionPushOutboxItem, err error) {
	s.retryCompletionPushAfter(ctx, outbox, item, err, 0)
}

func (s *serveServer) retryCompletionPushAfter(ctx context.Context, outbox session.CompletionPushOutboxStore, item session.CompletionPushOutboxItem, err error, requestedDelay time.Duration) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	delay := requestedDelay
	if delay <= 0 {
		delay = time.Second * time.Duration(1<<min(item.AttemptCount, 8))
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	_ = outbox.RetryCompletionPush(ctx, item.ID, time.Now().UTC().Add(delay), "temporary delivery failure")
}
