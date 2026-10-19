package cmd

import (
	"fmt"

	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/spf13/cobra"
)

var notifyTelegramCmd = &cobra.Command{
	Use:   "telegram --chat-id <id> <message>",
	Short: "Send a Telegram message and log it to the session store",
	Args:  cobra.ExactArgs(1),
	RunE:  runNotifyTelegram,
}

func init() {
	notifyTelegramCmd.Flags().Int64Var(&notifyTelegramChatID, "chat-id", 0, "Telegram chat ID to send to")
	notifyTelegramCmd.Flags().StringVar(&notifyTelegramParseMode, "parse-mode", "Markdown", "Telegram parse mode: Markdown or HTML")
	if err := notifyTelegramCmd.MarkFlagRequired("chat-id"); err != nil {
		panic(fmt.Sprintf("failed to mark chat-id required: %v", err))
	}

	notifyCmd.AddCommand(notifyTelegramCmd)
}

func runNotifyTelegram(cmd *cobra.Command, args []string) error {
	return appnotify.RunTelegram(cmd.Context(), notifyDeps(), appnotify.TelegramOptions{
		Message:   args[0],
		ChatID:    notifyTelegramChatID,
		ParseMode: notifyTelegramParseMode,
		Stderr:    cmd.ErrOrStderr(),
	})
}
