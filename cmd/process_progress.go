package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/process"
	"golang.org/x/term"
)

type processRestartRow struct {
	record  process.Record
	result  process.Record
	phase   string
	done    bool
	err     error
	elapsed time.Duration
}

type processRestartProgress struct {
	out     io.Writer
	width   int
	height  int
	lines   int
	timeout time.Duration
}

var processProgressTerminal = func(w io.Writer) (int, int, bool) {
	f, ok := w.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(f.Fd())) || os.Getenv("TERM") == "dumb" {
		return 0, 0, false
	}
	width, height, err := term.GetSize(int(f.Fd()))
	if err != nil || width < 2 || height < 2 {
		return 0, 0, false
	}
	return width, height, true
}

func newProcessRestartProgress(out io.Writer, records []process.Record, timeout time.Duration, noWait bool) *processRestartProgress {
	if noWait || len(records) == 0 {
		return nil
	}
	width, height, ok := processProgressTerminal(out)
	if !ok {
		return nil
	}
	return &processRestartProgress{out: out, width: width, height: height, timeout: timeout}
}

func processRestartPhase(phase string) string {
	switch phase {
	case "checking":
		return "Checking identity"
	case "waiting":
		return "Signal sent; waiting for acknowledgment"
	case "draining":
		return "Waiting for running tasks"
	case "cancelling":
		return "Stopping running tasks"
	case "replacing":
		return "Replacing executable"
	case "starting":
		return "Starting replacement"
	case "verifying", "ready":
		return "Verifying replacement"
	case "deferred":
		return "Reload deferred"
	default:
		return phase
	}
}

// Registry strings and errors are not terminal control sequences. Keep each row
// on one physical line so repaint cannot overwrite unrelated terminal output.
func processProgressText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(text))
}

func (p *processRestartProgress) render(rows []processRestartRow, elapsed time.Duration) {
	if f, ok := p.out.(interface{ Fd() uintptr }); ok {
		if width, height, err := term.GetSize(int(f.Fd())); err == nil && width > 1 && height > 1 {
			if width != p.width || height != p.height {
				// Resizing can reflow previous rows; do not rewind into that output.
				p.lines = 0
			}
			p.width, p.height = width, height
		}
	}
	var body strings.Builder
	completed, failed, stopped := 0, 0, 0
	for _, row := range rows {
		if row.done {
			completed++
			if errors.Is(row.err, process.ErrWaitStopped) {
				stopped++
			} else if row.err != nil {
				failed++
			}
		}
	}
	noun := "processes"
	if len(rows) == 1 {
		noun = "process"
	}
	fmt.Fprintf(&body, "Restarting %d %s | %ds elapsed | %d/%d finished | %d failed", len(rows), noun, int(elapsed.Seconds()), completed, len(rows), failed)
	if stopped > 0 {
		fmt.Fprintf(&body, " | %d wait ended", stopped)
	}
	body.WriteByte('\n')
	table := tabwriter.NewWriter(&body, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "PID\tMODE\tELAPSED\tSTATUS")
	for _, row := range rows {
		duration := elapsed
		state := processRestartPhase(row.phase)
		if row.done {
			duration = row.elapsed
			state = "Ready (replacement verified)"
			if row.err != nil {
				state = "FAILED: " + row.err.Error()
				if errors.Is(row.err, process.ErrWaitStopped) {
					state = "WAIT ENDED: restart not cancelled"
				}
			}
		}
		mode := row.record.Mode
		if mode == "" {
			mode = "-"
		}
		fmt.Fprintf(table, "%d\t%s\t%ds\t%s\n", row.record.PID, processProgressText(mode), int(duration.Seconds()), processProgressText(state))
	}
	_ = table.Flush()
	if completed < len(rows) {
		if p.timeout > 0 {
			fmt.Fprintf(&body, "CLI wait limit: %s; reaching it does not cancel the restart request.\n", p.timeout)
		}
	} else if failed > 0 || stopped > 0 {
		fmt.Fprintln(&body, "Not all replacements verified. See errors below; stopping the wait does not cancel a restart.")
	} else {
		fmt.Fprintln(&body, "All replacements verified ready.")
	}
	lines := strings.Split(strings.TrimSuffix(body.String(), "\n"), "\n")
	var frame strings.Builder
	// If the table exceeds the terminal height, append snapshots rather than
	// moving the cursor into scrollback. Every selected process remains visible.
	if p.lines > 0 && p.lines < p.height {
		fmt.Fprintf(&frame, "\x1b[%dA", p.lines)
	}
	for _, line := range lines {
		frame.WriteString("\r\x1b[2K")
		frame.WriteString(ansi.Truncate(line, p.width-1, "…"))
		frame.WriteByte('\n')
	}
	_, _ = io.WriteString(p.out, frame.String())
	p.lines = len(lines)
}
