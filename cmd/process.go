package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

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

type processRestartFunc func(context.Context, process.Record, bool, func(string)) (process.Record, error)

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
		records, err := list()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		sort.Slice(records, func(i, j int) bool { return records[i].PID < records[j].PID })
		var completions []string
		for _, record := range records {
			pid := strconv.Itoa(record.PID)
			if record.PID <= 0 || record.PID == self || !strings.HasPrefix(pid, prefix) {
				continue
			}
			// Cobra uses a tab-separated description where supported, and
			// strips it for shells that only accept the completion value.
			description := strings.Join(strings.Fields(processProgressText(record.Mode+" · "+record.Phase+" · "+record.Build)), " ")
			completions = append(completions, pid+"\t"+description)
		}
		return completions, cobra.ShellCompDirectiveNoFileComp
	}
	command.Flags().DurationVar(&timeout, "timeout", 0, "Limit CLI waiting only (0 waits until replacement); does not cancel a signalled restart")
	command.Flags().BoolVar(&noWait, "no-wait", false, "Return after signalling; does not confirm restart success")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if timeout < 0 {
			return errors.New("--timeout must not be negative")
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
		ctx := cmd.Context()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		progress := newProcessRestartProgress(cmd.ErrOrStderr(), selected, timeout, noWait)
		if _, _, terminal := processProgressTerminal(cmd.OutOrStdout()); !terminal {
			// Piping stdout must retain the existing machine-readable result lines.
			progress = nil
		}
		started := time.Now()
		rows := make([]processRestartRow, len(selected))
		for i, record := range selected {
			rows[i] = processRestartRow{record: record, phase: "checking"}
		}
		if progress != nil {
			progress.render(rows, 0)
		}
		type event struct {
			index  int
			phase  string
			record process.Record
			err    error
			done   bool
		}
		events := make(chan event, len(selected))
		// Snapshot once and signal concurrently. One event loop owns all output
		// and row state; no slow target hides completion of a faster target.
		for i, r := range selected {
			go func(i int, r process.Record) {
				var report func(string)
				if progress != nil {
					report = func(phase string) {
						select {
						case events <- event{index: i, phase: phase}:
						case <-ctx.Done():
						}
					}
				}
				next, err := restart(ctx, r, !noWait, report)
				events <- event{index: i, record: next, err: err, done: true}
			}(i, r)
		}
		var ticks <-chan time.Time
		if progress != nil {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			ticks = ticker.C
		}
		for remaining := len(selected); remaining > 0; {
			select {
			case update := <-events:
				row := &rows[update.index]
				if update.done {
					row.done, row.result, row.err = true, update.record, update.err
					row.elapsed = time.Since(started)
					remaining--
				} else {
					row.phase = update.phase
				}
			case <-ticks:
			}
			if progress != nil {
				progress.render(rows, time.Since(started))
			}
		}

		var failures []error
		for i, result := range rows {
			r := selected[i]
			if result.err != nil {
				if progress == nil {
					label := "failed"
					if errors.Is(result.err, process.ErrWaitStopped) {
						label = "wait ended (restart not cancelled)"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%v\n", r.PID, label, result.err)
				}
				failures = append(failures, fmt.Errorf("PID %d: %w", r.PID, result.err))
			} else if noWait {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tsignalled (not verified)\n", r.PID)
			} else if progress == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tready\t%s -> %s\t%s\n", r.PID, r.Instance, result.result.Instance, result.result.Build)
			}
		}
		return errors.Join(failures...)
	}
	return command
}
