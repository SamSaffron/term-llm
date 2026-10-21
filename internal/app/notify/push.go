package notify

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

// AppOptions configures `notify app`.
type AppOptions struct {
	Message string
	Stderr  io.Writer
}

// RunApp sends a Web Push notification to all subscribed apps.
func RunApp(ctx context.Context, deps Deps, opts AppOptions) error {
	return runPush(ctx, deps, opts.Message, opts.Stderr, session.PushClientApp)
}

func runPush(ctx context.Context, deps Deps, message string, errWriter io.Writer, clientKind string) error {
	cfg, err := deps.LoadConfig()
	if err != nil {
		return err
	}

	if !WebPushConfigured(cfg) {
		return fmt.Errorf("VAPID keys not configured (run 'term-llm serve web' to auto-generate)")
	}

	n, errs := sendWebPush(ctx, deps, cfg, message, errWriter, clientKind)
	if n == 0 && len(errs) == 0 {
		fmt.Fprintln(errWriter, "no push subscriptions found")
		return nil
	}
	if n > 0 {
		fmt.Fprintf(errWriter, "sent to %d subscription(s)\n", n)
	}
	for _, e := range errs {
		fmt.Fprintf(errWriter, "error: %s\n", e)
	}
	if len(errs) > 0 {
		return fmt.Errorf("notification errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
