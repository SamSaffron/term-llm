package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
)

var notifyWebCmd = &cobra.Command{
	Use:   "web <message>",
	Short: "Send a Web Push notification to all subscribed browsers",
	Args:  cobra.ExactArgs(1),
	RunE:  runNotifyWeb,
}

func init() {
	notifyCmd.AddCommand(notifyWebCmd)
}

func runNotifyWeb(cmd *cobra.Command, args []string) error {
	message := args[0]

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	if cfg.Serve.WebPush.VAPIDPublicKey == "" || cfg.Serve.WebPush.VAPIDPrivateKey == "" {
		return fmt.Errorf("VAPID keys not configured (run 'term-llm serve web' to auto-generate)")
	}

	n, errs := sendWebPushAll(cmd.Context(), cfg, message, cmd.ErrOrStderr())
	if n == 0 && len(errs) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no push subscriptions found")
		return nil
	}
	if n > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "sent to %d subscription(s)\n", n)
	}
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(cmd.ErrOrStderr(), "error: %s\n", e)
		}
	}
	return nil
}

func normalizeWebPushSubject(raw string) string {
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

// sendWebPushAll sends a push notification to all stored subscriptions.
// Returns the number of successful sends and a list of error strings.
func sendWebPushAll(ctx context.Context, cfg *config.Config, message string, errWriter io.Writer) (int, []string) {
	store, cleanup := InitSessionStore(cfg, errWriter)
	defer cleanup()
	if store == nil {
		return 0, []string{"session store not available"}
	}

	subs, err := store.ListPushSubscriptions(ctx)
	if err != nil {
		return 0, []string{fmt.Sprintf("list subscriptions: %v", err)}
	}
	if len(subs) == 0 {
		return 0, nil
	}

	payload, _ := json.Marshal(map[string]string{
		"title": "term-llm",
		"body":  message,
	})

	subject := normalizeWebPushSubject(cfg.Serve.WebPush.Subject)

	opts := &webpush.Options{
		VAPIDPublicKey:  cfg.Serve.WebPush.VAPIDPublicKey,
		VAPIDPrivateKey: cfg.Serve.WebPush.VAPIDPrivateKey,
		Subscriber:      subject,
		TTL:             60,
	}

	var errs []string
	sent := 0
	for _, sub := range subs {
		status, err := sendWebPush(ctx, &sub, payload, opts)
		if err != nil {
			errs = append(errs, fmt.Sprintf("push to %s: %v", truncateEndpoint(sub.Endpoint), err))
			continue
		}
		// Clean up stale subscriptions on 410 Gone or 404 Not Found
		if status == http.StatusGone || status == http.StatusNotFound {
			if delErr := store.DeletePushSubscription(ctx, sub.Endpoint); delErr != nil {
				log.Printf("warning: cleanup stale subscription: %v", delErr)
			}
			errs = append(errs, fmt.Sprintf("removed stale subscription %s", truncateEndpoint(sub.Endpoint)))
			continue
		}
		sent++
	}

	return sent, errs
}

// sendWebPush sends a single push notification and returns the HTTP status code.
func sendWebPush(ctx context.Context, sub *session.PushSubscription, payload []byte, opts *webpush.Options) (int, error) {
	s := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.KeyP256DH,
			Auth:   sub.KeyAuth,
		},
	}

	resp, err := webpush.SendNotificationWithContext(ctx, payload, s, opts)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("push failed: status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func truncateEndpoint(endpoint string) string {
	if len(endpoint) > 60 {
		return endpoint[:57] + "..."
	}
	return endpoint
}
