package ask

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

// WriteCommitEditMsg writes the commit message to .git/COMMIT_EDITMSG and .git/GITGUI_MSG.
func WriteCommitEditMsg(message string) error {
	gitInfo := tools.DetectGitRepo(".")
	if !gitInfo.IsRepo {
		return fmt.Errorf("not in a git repository")
	}
	message = strings.TrimSpace(message) + "\n"
	data := []byte(message)

	// Write to both files - COMMIT_EDITMSG for git commit, GITGUI_MSG for git gui
	commitPath := filepath.Join(gitInfo.Root, ".git", "COMMIT_EDITMSG")
	guiPath := filepath.Join(gitInfo.Root, ".git", "GITGUI_MSG")

	if err := os.WriteFile(commitPath, data, 0644); err != nil {
		return err
	}
	return os.WriteFile(guiPath, data, 0644)
}

type OnCompleteResult struct {
	Stdout string
	Stderr string
}

var (
	onCompleteTimeout   = 10 * time.Second
	onCompleteWaitDelay = 2 * time.Second
)

// RunOnCompleteCapture executes the on_complete shell command with input piped to stdin
// and captures stdout/stderr. Runs in the git repo root if available, else cwd.
func RunOnCompleteCapture(command, input string) (OnCompleteResult, error) {
	return RunOnCompleteCaptureInDir(command, input, ".")
}

// RunOnCompleteCaptureInDir executes an on_complete hook relative to baseDir,
// using that checkout's git root when available.
func RunOnCompleteCaptureInDir(command, input, baseDir string) (OnCompleteResult, error) {
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
		return OnCompleteResult{}, fmt.Errorf("on_complete setup failed: %w", prepErr)
	}
	defer cleanup()
	cmd.WaitDelay = onCompleteWaitDelay

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return OnCompleteResult{Stdout: stdout.String(), Stderr: stderr.String()}, fmt.Errorf("timed out after %s", onCompleteTimeout)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return OnCompleteResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
	}
	return OnCompleteResult{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

// RunOnComplete executes the on_complete shell command with input piped to stdin.
// Runs in the git repo root if available, else cwd.
func RunOnComplete(command, input string) error {
	result, err := RunOnCompleteCapture(command, input)
	if result.Stdout != "" {
		fmt.Fprint(os.Stdout, result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}
	return err
}

// TextCollector wraps an event channel and collects all text events.
type TextCollector struct {
	mu   sync.Mutex
	text strings.Builder
	done chan struct{}
}

// wrapEventsWithCollector creates a new channel that forwards all events from the input
// channel while collecting text events. Returns the wrapped channel.
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
