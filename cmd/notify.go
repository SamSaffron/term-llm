package cmd

import (
	"fmt"
	"strings"

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
	Long: `Send a notification message to all configured platforms (Telegram and Web Push
for apps and browsers). Use 'notify app' for app-only delivery or 'notify web'
for browser-only delivery. Push targets subscriptions in the configured session store.

Examples:
  term-llm notify "build finished"
  term-llm notify --chat-id 12345 "deploy complete"
  term-llm notify telegram --chat-id 12345 "test"
  term-llm notify app "test"
  term-llm notify web "test"`,
	Args:              validateNotifyMessage,
	ValidArgsFunction: cobra.NoFileCompletions,
	RunE:              runNotifyBroadcast,
}

func init() {
	notifyCmd.Flags().Int64Var(&notifyTelegramChatID, "chat-id", 0, "Telegram chat ID to send to")
	notifyCmd.Flags().StringVar(&notifyTelegramParseMode, "parse-mode", "Markdown", "Telegram parse mode: Markdown or HTML")

	registerNotifyCompletions(notifyCmd)

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

// validateNotifyMessage keeps missing-message errors useful even when root
// suppresses Cobra's automatic usage output.
func validateNotifyMessage(cmd *cobra.Command, args []string) error {
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		return nil
	}
	problem := "notification message is required"
	if len(args) > 1 {
		problem = "expected one notification message; wrap messages containing spaces in quotes"
	}
	example := cmd.CommandPath()
	if cmd.Name() == "telegram" {
		example += " --chat-id 12345"
	}
	return fmt.Errorf("%s\n\nUsage: %s\nExample: %s \"build finished\"", problem, cmd.UseLine(), example)
}

func registerNotifyCompletions(cmd *cobra.Command) {
	if err := cmd.RegisterFlagCompletionFunc("chat-id", cobra.NoFileCompletions); err != nil {
		panic(fmt.Sprintf("failed to register chat-id completion: %v", err))
	}
	if err := cmd.RegisterFlagCompletionFunc("parse-mode", staticCompletion("Markdown", "HTML")); err != nil {
		panic(fmt.Sprintf("failed to register parse-mode completion: %v", err))
	}
}
