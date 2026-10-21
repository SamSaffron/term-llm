package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

// WebPushConfigured reports whether both VAPID keys are configured, without
// resolving deferred values (op://, file://, $()).
func WebPushConfigured(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return cfg.Serve.WebPush.PublicKeyRef().Configured() && cfg.Serve.WebPush.PrivateKeyRef().Configured()
}

// WebPushKeys resolves the VAPID keypair on demand.
func WebPushKeys(cfg *config.Config) (publicKey string, privateKey string, err error) {
	if cfg == nil {
		return "", "", fmt.Errorf("web push is not configured")
	}
	publicKey, err = cfg.Serve.WebPush.PublicKeyRef().Resolve()
	if err != nil {
		return "", "", fmt.Errorf("serve.web_push.vapid_public_key: %w", err)
	}
	if publicKey == "" {
		return "", "", fmt.Errorf("serve.web_push.vapid_public_key resolved to an empty value")
	}
	privateKey, err = cfg.Serve.WebPush.PrivateKeyRef().Resolve()
	if err != nil {
		return "", "", fmt.Errorf("serve.web_push.vapid_private_key: %w", err)
	}
	if privateKey == "" {
		return "", "", fmt.Errorf("serve.web_push.vapid_private_key resolved to an empty value")
	}
	return publicKey, privateKey, nil
}

// WebPushPublicKey distinguishes an unavailable secret from an absent setting.
func WebPushPublicKey(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", nil
	}
	key, err := cfg.Serve.WebPush.PublicKeyRef().Resolve()
	if err != nil {
		return "", fmt.Errorf("serve.web_push.vapid_public_key: %w", err)
	}
	return key, nil
}

func WebPushKeyID(publicKey string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(publicKey)))
	return hex.EncodeToString(digest[:8])
}

func NormalizeWebPushSubject(raw string) string {
	subject := strings.TrimSpace(raw)
	if subject == "" {
		return "https://github.com/samsaffron/term-llm"
	}
	lower := strings.ToLower(subject)
	if strings.HasPrefix(lower, "mailto:") {
		return strings.TrimSpace(subject[len("mailto:"):])
	}
	return subject
}

// WebPushRecordSize keeps the encrypted body small enough for relays that wrap
// its base64 encoding in a 4096-byte APNs payload.
const WebPushRecordSize = 2048

// NewWebPushOptions applies the shared transport settings for all Web Push sends.
func NewWebPushOptions(publicKey, privateKey, subject string, ttl int) *webpush.Options {
	return &webpush.Options{
		VAPIDPublicKey:  publicKey,
		VAPIDPrivateKey: privateKey,
		Subscriber:      NormalizeWebPushSubject(subject),
		TTL:             ttl,
		RecordSize:      WebPushRecordSize,
	}
}

// SendWebPushAll sends a push notification to all stored subscriptions.
// Returns the number of successful sends and a list of error strings.
func SendWebPushAll(ctx context.Context, deps Deps, cfg *config.Config, message string, errWriter io.Writer) (int, []string) {
	return sendWebPush(ctx, deps, cfg, message, errWriter, "")
}

// sendWebPush filters by the stored client kind; an empty target broadcasts.
// Legacy subscriptions without a kind belong to browsers, never inferred from endpoints.
func sendWebPush(ctx context.Context, deps Deps, cfg *config.Config, message string, errWriter io.Writer, clientKind string) (int, []string) {
	store, cleanup := deps.InitSessionStore(cfg, errWriter)
	defer cleanup()
	if store == nil {
		return 0, []string{"session store not available"}
	}

	subs, err := store.ListPushSubscriptions(ctx)
	if err != nil {
		return 0, []string{fmt.Sprintf("list subscriptions: %v", err)}
	}
	matched := make([]session.PushSubscription, 0, len(subs))
	for _, sub := range subs {
		kind := sub.ClientKind
		if kind == "" {
			kind = session.PushClientBrowser
		}
		if sub.Status != "stale" && (clientKind == "" || kind == clientKind) {
			matched = append(matched, sub)
		}
	}
	if len(matched) == 0 {
		return 0, nil
	}

	payload, _ := json.Marshal(map[string]string{
		"title": "term-llm",
		"body":  message,
	})

	publicKey, privateKey, keyErr := WebPushKeys(cfg)
	if keyErr != nil {
		return 0, []string{keyErr.Error()}
	}

	opts := NewWebPushOptions(publicKey, privateKey, cfg.Serve.WebPush.Subject, 60)

	lifecycle, _ := session.AsPushSubscriptionLifecycleStore(store)
	var errs []string
	sent := 0
	for _, sub := range matched {
		status, err := SendWebPush(ctx, &sub, payload, opts)
		if status == http.StatusGone || status == http.StatusNotFound {
			if lifecycle != nil {
				if staleErr := lifecycle.MarkPushSubscriptionStale(ctx, sub.ID, fmt.Sprintf("http_%d", status), "push endpoint rejected the subscription"); staleErr != nil {
					errs = append(errs, fmt.Sprintf("mark stale subscription %s: %v", sub.ID, staleErr))
				}
			} else if delErr := store.DeletePushSubscription(ctx, sub.Endpoint); delErr != nil {
				errs = append(errs, fmt.Sprintf("remove stale subscription %s: %v", sub.ID, delErr))
			}
			if clientKind != "" {
				errs = append(errs, fmt.Sprintf("push to subscription %s: endpoint expired (HTTP %d); re-enroll notifications", sub.ID, status))
			}
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("push to subscription %s: %v", sub.ID, err))
			continue
		}
		if lifecycle != nil {
			_ = lifecycle.MarkPushSubscriptionUsed(ctx, sub.ID)
		}
		sent++
	}

	return sent, errs
}

// SendWebPush sends a single push notification and returns the HTTP status code.
func SendWebPush(ctx context.Context, sub *session.PushSubscription, payload []byte, opts *webpush.Options) (int, error) {
	status, _, err := SendWebPushDetailed(ctx, sub, payload, opts)
	return status, err
}

func SendWebPushDetailed(ctx context.Context, sub *session.PushSubscription, payload []byte, opts *webpush.Options) (int, time.Duration, error) {
	s := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.KeyP256DH,
			Auth:   sub.KeyAuth,
		},
	}

	resp, err := webpush.SendNotificationWithContext(ctx, payload, s, opts)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	var retryAfter time.Duration
	if raw := strings.TrimSpace(resp.Header.Get("Retry-After")); raw != "" {
		if seconds, parseErr := strconv.Atoi(raw); parseErr == nil && seconds > 0 {
			retryAfter = time.Duration(seconds) * time.Second
		} else if retryAt, dateErr := http.ParseTime(raw); dateErr == nil {
			retryAfter = time.Until(retryAt)
		}
		if retryAfter < 0 {
			retryAfter = 0
		}
		if retryAfter > time.Hour {
			retryAfter = time.Hour
		}
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, retryAfter, fmt.Errorf("push failed: status %d", resp.StatusCode)
	}
	return resp.StatusCode, retryAfter, nil
}
