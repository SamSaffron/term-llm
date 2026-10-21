package cmd

import (
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/spf13/cobra"
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
