package cmd

import (
	"fmt"
	"strings"

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
	ctx := cmd.Context()
	message := args[0]
	parseMode, err := normalizeTelegramParseMode(notifyTelegramParseMode)
	if err != nil {
		return err
	}

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	token := strings.TrimSpace(cfg.Serve.Telegram.Token)
	if token == "" {
		return fmt.Errorf("telegram token is not configured (serve.telegram.token)")
	}

	if err := sendTelegramMessage(ctx, token, notifyTelegramChatID, message, parseMode); err != nil {
		return err
	}

	logTelegramNotifySession(ctx, cfg, notifyTelegramChatID, message, cmd.ErrOrStderr())
	return nil
}
