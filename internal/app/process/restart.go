package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/samsaffron/term-llm/internal/process"
)

// RestartFunc restarts one process, reporting phases through the callback.
type RestartFunc func(context.Context, process.Record, bool, func(string)) (process.Record, error)

// RestartOptions configures Restart. All restarts every registered process
// other than Self; otherwise only PID is restarted.
type RestartOptions struct {
	Out, Err io.Writer
	All      bool
	PID      int
	Self     int
	Timeout  time.Duration
	NoWait   bool
	List     ListFunc
	Restart  RestartFunc
}

// ParsePID validates the PID argument of `process restart`.
func ParsePID(arg string, self int) (int, error) {
	pid, err := strconv.Atoi(arg)
	if err != nil || pid <= 0 {
		return 0, errors.New("PID must be a positive integer")
	}
	if pid == self {
		return 0, errors.New("refusing to restart self")
	}
	return pid, nil
}

type restartEvent struct {
	index  int
	phase  string
	record process.Record
	err    error
	done   bool
}

// Restart signals the selected processes concurrently, renders progress when
// stdout and stderr allow it, and prints one result line per process otherwise.
func Restart(ctx context.Context, o RestartOptions) error {
	if o.Timeout < 0 {
		return errors.New("--timeout must not be negative")
	}
	records, err := o.List()
	if err != nil {
		return err
	}
	selected := make([]process.Record, 0, len(records))
	for _, r := range records {
		if r.PID != o.Self && (o.All || r.PID == o.PID) {
			selected = append(selected, r)
		}
	}
	if len(selected) == 0 {
		if !o.All {
			return fmt.Errorf("PID %d has no validated process record", o.PID)
		}
		fmt.Fprintln(o.Out, "No registered processes to restart.")
		return nil
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].PID < selected[j].PID })
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	progress := newRestartProgress(o.Err, selected, o.Timeout, o.NoWait)
	if _, _, terminal := progressTerminal(o.Out); !terminal {
		// Piping stdout must retain the existing machine-readable result lines.
		progress = nil
	}
	started := time.Now()
	rows := make([]restartRow, len(selected))
	for i, record := range selected {
		rows[i] = restartRow{record: record, phase: "checking"}
	}
	if progress != nil {
		progress.render(rows, 0)
	}
	events := make(chan restartEvent, len(selected))
	// Snapshot once and signal concurrently. One event loop owns all output
	// and row state; no slow target hides completion of a faster target.
	for i, r := range selected {
		go func(i int, r process.Record) {
			var report func(string)
			if progress != nil {
				report = func(phase string) {
					select {
					case events <- restartEvent{index: i, phase: phase}:
					case <-ctx.Done():
					}
				}
			}
			next, err := o.Restart(ctx, r, !o.NoWait, report)
			events <- restartEvent{index: i, record: next, err: err, done: true}
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
	return reportResults(o, selected, rows, progress != nil)
}

func reportResults(o RestartOptions, selected []process.Record, rows []restartRow, rendered bool) error {
	var failures []error
	for i, result := range rows {
		r := selected[i]
		if result.err != nil {
			if !rendered {
				label := "failed"
				if errors.Is(result.err, process.ErrWaitStopped) {
					label = "wait ended (restart not cancelled)"
				}
				fmt.Fprintf(o.Out, "%d\t%s\t%v\n", r.PID, label, result.err)
			}
			failures = append(failures, fmt.Errorf("PID %d: %w", r.PID, result.err))
		} else if o.NoWait {
			fmt.Fprintf(o.Out, "%d\tsignalled (not verified)\n", r.PID)
		} else if !rendered {
			fmt.Fprintf(o.Out, "%d\tready\t%s -> %s\t%s\n", r.PID, r.Instance, result.result.Instance, result.result.Build)
		}
	}
	return errors.Join(failures...)
}
