package cmd

import (
	"os"
	"time"

	appprocess "github.com/samsaffron/term-llm/internal/app/process"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/spf13/cobra"
)

func newProcessCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "process", Short: "Inspect and restart local term-llm processes"}
	cmd.AddCommand(newProcessListCommand(process.List, os.Getpid()))
	cmd.AddCommand(newProcessRestartCommand(false, process.List, process.RestartWithProgress, os.Getpid()))
	cmd.AddCommand(newProcessRestartCommand(true, process.List, process.RestartWithProgress, os.Getpid()))
	return cmd
}

func newProcessListCommand(list func() ([]process.Record, error), self int) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "list", Short: "List validated local instances and clean stale records",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return appprocess.List(cmd.OutOrStdout(), list, self, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print full process records as JSON")
	return cmd
}

func init() { rootCmd.AddCommand(newProcessCommand()) }

type processRestartFunc = appprocess.RestartFunc

func newProcessRestartCommand(all bool, list func() ([]process.Record, error), restart processRestartFunc, self int) *cobra.Command {
	var timeout time.Duration
	var noWait bool
	command := &cobra.Command{Use: "restart PID", Short: "Restart one registered instance and wait for the installed build", Args: cobra.ExactArgs(1)}
	if all {
		command.Use = "restart-all"
		command.Short = "Restart a snapshot of your registered instances and wait for each"
		command.Args = cobra.NoArgs
	}
	command.ValidArgsFunction = func(_ *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if all || len(args) != 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return appprocess.CompletePIDs(list, self, prefix), cobra.ShellCompDirectiveNoFileComp
	}
	command.Flags().DurationVar(&timeout, "timeout", 0, "Limit CLI waiting only (0 waits until replacement); does not cancel a signalled restart")
	command.Flags().BoolVar(&noWait, "no-wait", false, "Return after signalling; does not confirm restart success")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		opts := appprocess.RestartOptions{
			Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(), All: all, Self: self,
			Timeout: timeout, NoWait: noWait, List: list, Restart: restart,
		}
		if timeout < 0 {
			return appprocess.Restart(cmd.Context(), opts) // reports the timeout error
		}
		if !all {
			pid, err := appprocess.ParsePID(args[0], self)
			if err != nil {
				return err
			}
			opts.PID = pid
		}
		return appprocess.Restart(cmd.Context(), opts)
	}
	return command
}
