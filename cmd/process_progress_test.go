package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/process"
)

func TestProcessRestartProgressFrames(t *testing.T) {
	var output bytes.Buffer
	p := &processRestartProgress{out: &output, width: 100, height: 30, timeout: 2 * time.Minute}
	rows := []processRestartRow{{record: process.Record{PID: 10, Mode: "serve web"}, phase: "draining"}, {record: process.Record{PID: 30, Mode: "chat"}, phase: "starting"}}
	p.render(rows, 3*time.Second)
	for _, want := range []string{"10", "serve web", "30", "chat", "3s", "Waiting for running tasks", "Starting replacement", "0/2 finished"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q: %s", want, output.String())
		}
	}
	output.Reset()
	rows[1].done = true
	rows[1].elapsed = 4 * time.Second
	p.render(rows, 9*time.Second)
	text := ansi.Strip(output.String())
	if !strings.Contains(output.String(), "\x1b[5A") || !strings.Contains(text, "1/2 finished") || !strings.Contains(text, "Ready (replacement verified)") || !strings.Contains(text, "4s") || !strings.Contains(text, "9s") {
		t.Fatal(output.String())
	}
	output.Reset()
	rows[0].done = true
	rows[0].err = errors.New("reload drain: context deadline exceeded")
	rows[0].elapsed = 30 * time.Second
	p.render(rows, 30*time.Second)
	if !strings.Contains(output.String(), "2/2 finished | 1 failed") || !strings.Contains(output.String(), "FAILED: reload drain") {
		t.Fatal(output.String())
	}
}

func TestProcessProgressDoesNotWrapOrInjectControlSequences(t *testing.T) {
	var output bytes.Buffer
	p := &processRestartProgress{out: &output, width: 35, height: 3, timeout: time.Minute}
	rows := []processRestartRow{{record: process.Record{PID: 10, Mode: "web\x1b[2J\nforged row"}, done: true, err: errors.New(strings.Repeat("wide界", 30))}}
	p.render(rows, time.Second)
	p.render(rows, 2*time.Second)
	if strings.Contains(output.String(), "\x1b[2J") || strings.Contains(output.String(), "\x1b[4A") {
		t.Fatal("unsafe control sequence or scrollback repaint", output.String())
	}
	for _, line := range strings.Split(ansi.Strip(output.String()), "\n") {
		if ansi.StringWidth(line) > 34 {
			t.Fatalf("wrapped line: %q", line)
		}
	}
}

// Run the actual event loop with fake terminal writers. The target is held until
// a one-second redraw arrives, proving progress is emitted before completion.
func TestProcessRestartTerminalUpdatesWhileWaiting(t *testing.T) {
	original := processProgressTerminal
	processProgressTerminal = func(io.Writer) (int, int, bool) { return 120, 40, true }
	defer func() { processProgressTerminal = original }()
	frames := &processProgressTestWriter{updates: make(chan string, 32)}
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := newProcessRestartCommand(true, func() ([]process.Record, error) {
		return []process.Record{{PID: 10, Mode: "serve web"}, {PID: 30, Mode: "chat"}}, nil
	}, func(ctx context.Context, r process.Record, wait bool, progress func(string)) (process.Record, error) {
		if progress == nil || !wait {
			return r, errors.New("expected progress and readiness wait")
		}
		progress("draining")
		if r.PID == 30 {
			return r, nil
		}
		select {
		case <-release:
		case <-ctx.Done():
			return r, ctx.Err()
		}
		return r, errors.New("reload drain: context deadline exceeded")
	}, 99)
	command.SetContext(ctx)
	command.SetArgs(nil)
	command.SetOut(io.Discard)
	command.SetErr(frames)
	command.SilenceUsage, command.SilenceErrors = true, true
	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	found := false
	for !found {
		select {
		case frame := <-frames.updates:
			found = strings.Contains(frame, "1s elapsed") && strings.Contains(frame, "1/2 finished") && strings.Contains(frame, "Waiting for running tasks")
		case <-deadline.C:
			cancel()
			<-done
			t.Fatal("no elapsed progress while a target remained outstanding")
		}
	}
	close(release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "PID 10") {
		t.Fatal("failure not propagated:", err)
	}
}

type processProgressTestWriter struct{ updates chan string }

func (w *processProgressTestWriter) Write(p []byte) (int, error) {
	w.updates <- string(p)
	return len(p), nil
}

func TestProcessRestartRedirectedOutputStaysPlain(t *testing.T) {
	original := processProgressTerminal
	var stdout, stderr bytes.Buffer
	processProgressTerminal = func(w io.Writer) (int, int, bool) { return 100, 30, w == &stderr }
	defer func() { processProgressTerminal = original }()
	command := newProcessRestartCommand(false, func() ([]process.Record, error) { return []process.Record{{PID: 10}}, nil }, func(_ context.Context, r process.Record, _ bool, progress func(string)) (process.Record, error) {
		if progress != nil {
			t.Error("progress enabled with piped stdout")
		}
		return r, nil
	}, 99)
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"10"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 || !strings.HasPrefix(stdout.String(), "10\tready") || strings.Contains(stdout.String(), "\x1b") {
		t.Fatal(stdout.String(), stderr.String())
	}
}

func TestProcessProgressNoWaitDoesNotAnimate(t *testing.T) {
	original := processProgressTerminal
	processProgressTerminal = func(io.Writer) (int, int, bool) { return 100, 30, true }
	defer func() { processProgressTerminal = original }()
	if p := newProcessRestartProgress(io.Discard, []process.Record{{PID: 10}}, time.Minute, true); p != nil {
		t.Fatal("no-wait started progress")
	}
}

func TestProcessProgressDefaultPendingAndExplicitWaitStopped(t *testing.T) {
	var output bytes.Buffer
	p := &processRestartProgress{out: &output, width: 150, height: 30}
	rows := []processRestartRow{{record: process.Record{PID: 10}, phase: "draining"}}
	p.render(rows, 31*time.Second)
	for _, jargon := range []string{"Safe-point", "eligible work", "then joined", "No automatic timeout"} {
		if strings.Contains(output.String(), jargon) {
			t.Fatalf("internal reload jargon leaked into progress: %s", output.String())
		}
	}
	if !strings.Contains(output.String(), "Waiting for running tasks") || !strings.Contains(output.String(), "31s") {
		t.Fatal(output.String())
	}
	output.Reset()
	rows[0].done = true
	rows[0].err = process.ErrWaitStopped
	p.render(rows, 31*time.Second)
	for _, want := range []string{"0 failed", "1 wait ended", "WAIT ENDED: restart not cancelled"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "All replacements verified ready") {
		t.Fatal("wait expiration reported as success")
	}
}
