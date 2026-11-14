package cmd

import (
	"github.com/spf13/cobra"
	appnotify "github.com/term-llm/term-llm/internal/app/notify"
)

var notifyAppCmd = &cobra.Command{
	Use:               "app <message>",
	Short:             "Send a Web Push notification to all subscribed apps",
	Args:              validateNotifyMessage,
	ValidArgsFunction: cobra.NoFileCompletions,
	RunE:              runNotifyApp,
}

func init() {
	notifyCmd.AddCommand(notifyAppCmd)
}

func runNotifyApp(cmd *cobra.Command, args []string) error {
	return appnotify.RunApp(cmd.Context(), notifyDeps(), appnotify.AppOptions{
		Message: args[0],
		Stderr:  cmd.ErrOrStderr(),
	})
}
