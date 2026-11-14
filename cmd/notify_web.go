package cmd

import (
	"github.com/spf13/cobra"
	appnotify "github.com/term-llm/term-llm/internal/app/notify"
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
