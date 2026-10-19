// Package oncomplete runs configured completion hooks.
package oncomplete

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// Result contains output captured from an on-complete hook.
type Result struct {
	Stdout string
	Stderr string
}

var (
	onCompleteTimeout   = 10 * time.Second
	onCompleteWaitDelay = 2 * time.Second
)

// RunCapture executes the on_complete shell command with input piped to stdin
// and captures stdout/stderr. It runs in the git repo root when available.
func RunCapture(command, input string) (Result, error) {
	return RunCaptureInDir(command, input, ".")
}

// RunCaptureInDir executes an on_complete hook relative to baseDir, using that
// checkout's git root when available.
func RunCaptureInDir(command, input, baseDir string) (Result, error) {
	dir := strings.TrimSpace(baseDir)
	if dir == "" {
		dir = "."
	}
	if gitInfo := tools.DetectGitRepo(dir); gitInfo.IsRepo {
		dir = gitInfo.Root
	}

	ctx, cancel := context.WithTimeout(context.Background(), onCompleteTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)

	cleanup, prepErr := tools.PrepareCommand(cmd)
	if prepErr != nil {
		return Result{}, fmt.Errorf("on_complete setup failed: %w", prepErr)
	}
	defer cleanup()
	cmd.WaitDelay = onCompleteWaitDelay

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Result{Stdout: stdout.String(), Stderr: stderr.String()}, fmt.Errorf("timed out after %s", onCompleteTimeout)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return Result{Stdout: stdout.String(), Stderr: stderr.String()}, nil
	}
	return Result{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

// WriteCommitEditMsg writes the commit message to .git/COMMIT_EDITMSG and .git/GITGUI_MSG.
func WriteCommitEditMsg(message string) error {
	gitInfo := tools.DetectGitRepo(".")
	if !gitInfo.IsRepo {
		return fmt.Errorf("not in a git repository")
	}
	message = strings.TrimSpace(message) + "\n"
	data := []byte(message)

	commitPath := filepath.Join(gitInfo.Root, ".git", "COMMIT_EDITMSG")
	guiPath := filepath.Join(gitInfo.Root, ".git", "GITGUI_MSG")
	if err := os.WriteFile(commitPath, data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(guiPath, data, 0o644)
}

// TextCollector wraps an event channel and collects all text events.
type TextCollector struct {
	mu   sync.Mutex
	text strings.Builder
	done chan struct{}
}

// NewTextCollector returns an empty event text collector.
func NewTextCollector() *TextCollector { return &TextCollector{} }

// WrapEvents forwards events while collecting text and respecting discarded attempts.
func (tc *TextCollector) WrapEvents(events <-chan ui.StreamEvent) <-chan ui.StreamEvent {
	wrapped := make(chan ui.StreamEvent, 100)
	tc.done = make(chan struct{})
	go func() {
		defer close(wrapped)
		defer close(tc.done)
		for ev := range events {
			switch ev.Type {
			case ui.StreamEventText:
				tc.mu.Lock()
				tc.text.WriteString(ev.Text)
				tc.mu.Unlock()
			case ui.StreamEventAttemptDiscard:
				tc.mu.Lock()
				tc.text.Reset()
				tc.mu.Unlock()
			}
			wrapped <- ev
		}
	}()
	return wrapped
}

// Wait blocks until all wrapped events have been collected.
func (tc *TextCollector) Wait() {
	if tc == nil || tc.done == nil {
		return
	}
	<-tc.done
}

// Text returns the collected text.
func (tc *TextCollector) Text() string {
	if tc == nil {
		return ""
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.text.String()
}

// Run executes the on_complete shell command with input piped to stdin. It
// writes captured output to the process streams.
func Run(command, input string) error {
	result, err := RunCapture(command, input)
	if result.Stdout != "" {
		fmt.Fprint(os.Stdout, result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}
	return err
}
