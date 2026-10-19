// Package notify implements the `term-llm notify` command family. The cmd
// package owns Cobra definitions and flag binding; each command here takes an
// explicit Options struct and reaches CLI-level helpers through Deps.
package notify

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

// Deps are CLI-level helpers owned by the cmd package.
type Deps struct {
	LoadConfig       func() (*config.Config, error)
	InitSessionStore func(cfg *config.Config, errWriter io.Writer) (session.Store, func())
}

// BroadcastOptions configures `notify <message>`.
type BroadcastOptions struct {
	Message   string
	ChatID    int64
	ParseMode string
	Stderr    io.Writer
}

// TelegramOptions configures `notify telegram`.
type TelegramOptions struct {
	Message   string
	ChatID    int64
	ParseMode string
	Stderr    io.Writer
}

// WebOptions configures `notify web`.
type WebOptions struct {
	Message string
	Stderr  io.Writer
}

// RunBroadcast sends the message to every configured platform.
func RunBroadcast(ctx context.Context, deps Deps, opts BroadcastOptions) error {
	cfg, err := deps.LoadConfig()
	if err != nil {
		return err
	}

	var errs []string
	sent := 0

	// Telegram: send if token is configured and chat-id provided. Resolution
	// failures are reported alongside other platform errors rather than
	// aborting web push.
	token := ""
	if opts.ChatID != 0 && cfg.Serve.Telegram.TokenRef().Configured() {
		resolved, tokenErr := cfg.Serve.Telegram.TokenRef().Resolve()
		if tokenErr != nil {
			errs = append(errs, fmt.Sprintf("serve.telegram.token: %v", tokenErr))
		}
		token = resolved
	}
	if token != "" && opts.ChatID != 0 {
		parseMode, err := NormalizeTelegramParseMode(opts.ParseMode)
		if err != nil {
			return err
		}
		if err := SendTelegramMessage(ctx, token, opts.ChatID, opts.Message, parseMode); err != nil {
			errs = append(errs, fmt.Sprintf("telegram: %v", err))
		} else {
			LogTelegramNotifySession(ctx, deps, cfg, opts.ChatID, opts.Message, opts.Stderr)
			fmt.Fprintln(opts.Stderr, "sent via telegram")
			sent++
		}
	}

	// Web Push: send if VAPID keys are configured
	if WebPushConfigured(cfg) {
		n, webErrs := SendWebPushAll(ctx, deps, cfg, opts.Message, opts.Stderr)
		sent += n
		errs = append(errs, webErrs...)
		if n > 0 {
			fmt.Fprintf(opts.Stderr, "sent via web push (%d subscriptions)\n", n)
		}
	}

	if sent == 0 && len(errs) == 0 {
		return fmt.Errorf("no notification platforms configured\n\nConfigure telegram (serve.telegram.token + --chat-id) or web push (serve.web_push VAPID keys)")
	}
	if len(errs) > 0 {
		return fmt.Errorf("notification errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// RunTelegram sends a Telegram message and logs it to the session store.
func RunTelegram(ctx context.Context, deps Deps, opts TelegramOptions) error {
	parseMode, err := NormalizeTelegramParseMode(opts.ParseMode)
	if err != nil {
		return err
	}

	cfg, err := deps.LoadConfig()
	if err != nil {
		return err
	}

	token, err := cfg.Serve.Telegram.TokenRef().Resolve()
	if err != nil {
		return fmt.Errorf("serve.telegram.token: %w", err)
	}
	if token == "" {
		return fmt.Errorf("telegram token is not configured (serve.telegram.token)")
	}

	if err := SendTelegramMessage(ctx, token, opts.ChatID, opts.Message, parseMode); err != nil {
		return err
	}

	LogTelegramNotifySession(ctx, deps, cfg, opts.ChatID, opts.Message, opts.Stderr)
	return nil
}

// RunWeb sends a Web Push notification to all subscribed browsers.
func RunWeb(ctx context.Context, deps Deps, opts WebOptions) error {
	cfg, err := deps.LoadConfig()
	if err != nil {
		return err
	}

	if !WebPushConfigured(cfg) {
		return fmt.Errorf("VAPID keys not configured (run 'term-llm serve web' to auto-generate)")
	}

	n, errs := SendWebPushAll(ctx, deps, cfg, opts.Message, opts.Stderr)
	if n == 0 && len(errs) == 0 {
		fmt.Fprintln(opts.Stderr, "no push subscriptions found")
		return nil
	}
	if n > 0 {
		fmt.Fprintf(opts.Stderr, "sent to %d subscription(s)\n", n)
	}
	for _, e := range errs {
		fmt.Fprintf(opts.Stderr, "error: %s\n", e)
	}
	return nil
}
