package cmd

import (
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/spf13/cobra"
)

var notifyWebCmd = &cobra.Command{
	Use:               "web <message>",
	Short:             "Send a Web Push notification to all subscribed browsers",
	Args:              validateNotifyMessage,
	ValidArgsFunction: cobra.NoFileCompletions,
	RunE:              runNotifyWeb,
}

func init() {
	notifyCmd.AddCommand(notifyWebCmd)
}

func runNotifyWeb(cmd *cobra.Command, args []string) error {
	return appnotify.RunWeb(cmd.Context(), notifyDeps(), appnotify.WebOptions{
		Message: args[0],
		Stderr:  cmd.ErrOrStderr(),
	})
}
