package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/samsaffron/term-llm/internal/process"
	"github.com/spf13/cobra"
)

func newProcessCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "process", Short: "Inspect and restart local term-llm processes"}
	cmd.AddCommand(newProcessListCommand(process.List, os.Getpid()))
	cmd.AddCommand(newProcessRestartCommand(false, process.List, process.Restart, os.Getpid()))
	cmd.AddCommand(newProcessRestartCommand(true, process.List, process.Restart, os.Getpid()))
	return cmd
}

func newProcessListCommand(list func() ([]process.Record, error), self int) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "list", Short: "List validated local instances and clean stale records",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			records, err := list()
			if err != nil {
				return err
			}
			visible := make([]process.Record, 0, len(records))
			for _, record := range records {
				if record.PID != self {
					visible = append(visible, record)
				}
			}
			sort.Slice(visible, func(i, j int) bool { return visible[i].PID < visible[j].PID })
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(visible)
			}
			out := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(out, "PID\tMODE\tINSTANCE\tBUILD\tSTATE")
			for _, r := range visible {
				fmt.Fprintf(out, "%d\t%s\t%s\t%s\t%s\n", r.PID, r.Mode, r.Instance, r.Build, r.Phase)
			}
			return out.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print full process records as JSON")
	return cmd
}

func init() { rootCmd.AddCommand(newProcessCommand()) }

type processRestartFunc func(context.Context, process.Record, bool) (process.Record, error)

func newProcessRestartCommand(all bool, list func() ([]process.Record, error), restart processRestartFunc, self int) *cobra.Command {
	var timeout time.Duration
	var noWait bool
	command := &cobra.Command{Use: "restart PID", Short: "Restart one registered instance and wait for the installed build", Args: cobra.ExactArgs(1)}
	if all {
		command.Use = "restart-all"
		command.Short = "Restart a snapshot of your registered instances and wait for each"
		command.Args = cobra.NoArgs
	}
	command.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "Maximum time to wait for replacement readiness")
	command.Flags().BoolVar(&noWait, "no-wait", false, "Return after signalling; does not confirm restart success")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if timeout <= 0 {
			return errors.New("--timeout must be positive")
		}
		var pid int
		if !all {
			var err error
			pid, err = strconv.Atoi(args[0])
			if err != nil || pid <= 0 {
				return errors.New("PID must be a positive integer")
			}
			if pid == self {
				return errors.New("refusing to restart self")
			}
		}
		records, err := list()
		if err != nil {
			return err
		}
		selected := make([]process.Record, 0, len(records))
		for _, r := range records {
			if r.PID != self && (all || r.PID == pid) {
				selected = append(selected, r)
			}
		}
		if len(selected) == 0 {
			if !all {
				return fmt.Errorf("PID %d has no validated process record", pid)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "No registered processes to restart.")
			return nil
		}
		sort.Slice(selected, func(i, j int) bool { return selected[i].PID < selected[j].PID })
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		type result struct {
			record process.Record
			err    error
		}
		results := make([]result, len(selected))
		// Snapshot once and request concurrently: waiting for the first process must
		// not postpone signalling the others. Output remains serialized and ordered.
		var wg sync.WaitGroup
		for i, r := range selected {
			wg.Add(1)
			go func(i int, r process.Record) {
				defer wg.Done()
				results[i].record, results[i].err = restart(ctx, r, !noWait)
			}(i, r)
		}
		wg.Wait()
		var failures []error
		for i, result := range results {
			r := selected[i]
			if result.err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tfailed\t%v\n", r.PID, result.err)
				failures = append(failures, fmt.Errorf("PID %d: %w", r.PID, result.err))
			} else if noWait {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tsignalled (not verified)\n", r.PID)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tready\t%s -> %s\t%s\n", r.PID, r.Instance, result.record.Instance, result.record.Build)
			}
		}
		return errors.Join(failures...)
	}
	return command
}
