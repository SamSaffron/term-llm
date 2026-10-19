package cmd

import (
	"context"
	"io"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
)

var (
	notifyTelegramChatID    int64
	notifyTelegramParseMode string
)

var notifyCmd = &cobra.Command{
	Use:   "notify <message>",
	Short: "Send a notification to all configured platforms",
	Long: `Send a notification message to all configured platforms (Telegram, Web Push).

Examples:
  term-llm notify "build finished"
  term-llm notify --chat-id 12345 "deploy complete"
  term-llm notify telegram --chat-id 12345 "test"
  term-llm notify web "test"`,
	Args: cobra.ExactArgs(1),
	RunE: runNotifyBroadcast,
}

func init() {
	notifyCmd.Flags().Int64Var(&notifyTelegramChatID, "chat-id", 0, "Telegram chat ID to send to")
	notifyCmd.Flags().StringVar(&notifyTelegramParseMode, "parse-mode", "Markdown", "Telegram parse mode: Markdown or HTML")

	rootCmd.AddCommand(notifyCmd)
}

func notifyDeps() appnotify.Deps {
	return appnotify.Deps{LoadConfig: loadConfigWithSetup, InitSessionStore: InitSessionStore}
}

func runNotifyBroadcast(cmd *cobra.Command, args []string) error {
	return appnotify.RunBroadcast(cmd.Context(), notifyDeps(), appnotify.BroadcastOptions{
		Message:   args[0],
		ChatID:    notifyTelegramChatID,
		ParseMode: notifyTelegramParseMode,
		Stderr:    cmd.ErrOrStderr(),
	})
}

// Thin aliases used by serve code that still lives in cmd.

type telegramSendResponse = appnotify.TelegramSendResponse

func sendTelegramMessage(ctx context.Context, token string, chatID int64, message, parseMode string) error {
	return appnotify.SendTelegramMessage(ctx, token, chatID, message, parseMode)
}

func logTelegramNotifySession(ctx context.Context, cfg *config.Config, chatID int64, message string, errWriter io.Writer) {
	appnotify.LogTelegramNotifySession(ctx, notifyDeps(), cfg, chatID, message, errWriter)
}

func webPushConfigured(cfg *config.Config) bool { return appnotify.WebPushConfigured(cfg) }

func webPushKeys(cfg *config.Config) (string, string, error) { return appnotify.WebPushKeys(cfg) }

func webPushPublicKey(cfg *config.Config) (string, error) { return appnotify.WebPushPublicKey(cfg) }

func webPushKeyID(publicKey string) string { return appnotify.WebPushKeyID(publicKey) }

func normalizeWebPushSubject(raw string) string { return appnotify.NormalizeWebPushSubject(raw) }

func sendWebPush(ctx context.Context, sub *session.PushSubscription, payload []byte, opts *webpush.Options) (int, error) {
	return appnotify.SendWebPush(ctx, sub, payload, opts)
}

func sendWebPushDetailed(ctx context.Context, sub *session.PushSubscription, payload []byte, opts *webpush.Options) (int, time.Duration, error) {
	return appnotify.SendWebPushDetailed(ctx, sub, payload, opts)
}
