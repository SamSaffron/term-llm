package cmd

import (
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"

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
