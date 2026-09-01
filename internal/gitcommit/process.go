package gitcommit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

var scrubGitEnvironment = map[string]struct{}{
	"GIT_DIR": {}, "GIT_COMMON_DIR": {}, "GIT_WORK_TREE": {}, "GIT_INDEX_FILE": {},
	"GIT_OBJECT_DIRECTORY": {}, "GIT_ALTERNATE_OBJECT_DIRECTORIES": {}, "GIT_NAMESPACE": {},
	"GIT_PREFIX": {}, "GIT_QUARANTINE_PATH": {},
}

func gitEnvironment(extra map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		drop := false
		for scrubbed := range scrubGitEnvironment {
			if strings.EqualFold(key, scrubbed) {
				drop = true
				break
			}
		}
		if !drop {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_LITERAL_PATHSPECS=1", "GIT_TERMINAL_PROMPT=0")
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	return env
}

type commandError struct {
	args           []string
	stdout, stderr string
	err            error
}

func (e *commandError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, strings.TrimSpace(e.stderr))
}
func (e *commandError) Unwrap() error { return e.err }

type limitBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := b.max - b.buf.Len()
	if left > 0 {
		if len(p) > left {
			b.buf.Write(p[:left])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}
func (b *limitBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}
func (b *limitBuffer) String() string { return string(b.Bytes()) }

func runGitAt(ctx context.Context, dir string, extra map[string]string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, &Error{Kind: ErrGitMissing, Message: "Git is not installed or is not on PATH", Cause: err}
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnvironment(extra)
	out := &limitBuffer{max: maxCommandOutput}
	stderr := &limitBuffer{max: maxCommandOutput}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err := cmd.Run()
	if out.truncated || stderr.truncated {
		return nil, &Error{Kind: ErrOutputLimit, Message: "Git command output exceeded the supported limit", Output: stderr.String(), Cause: err}
	}
	if err != nil {
		return nil, &commandError{args: args, stdout: out.String(), stderr: stderr.String(), err: err}
	}
	return out.Bytes(), nil
}
func (r *Repository) git(ctx context.Context, extra map[string]string, args ...string) ([]byte, error) {
	return runGitAt(ctx, r.root, extra, args...)
}

func classifyDiscovery(err error) error {
	if IsKind(err, ErrGitMissing) {
		return err
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "dubious ownership") || strings.Contains(text, "safe.directory"):
		return typed(ErrUnsafeRepository, "Git refused this repository because its ownership is unsafe", err)
	case strings.Contains(text, "not a git repository") || strings.Contains(text, "not a git directory"):
		return typed(ErrNotRepository, "the active session directory is not in a Git checkout", err)
	default:
		return typed(ErrNotRepository, "cannot open the active Git checkout", err)
	}
}
func classifyGitError(action string, err error) error {
	if err == nil {
		return nil
	}
	if IsKind(err, ErrOutputLimit) || IsKind(err, ErrGitMissing) {
		return err
	}
	text := strings.ToLower(err.Error())
	kind := ErrCommit
	message := action + " failed"
	switch {
	case strings.Contains(text, "index.lock") || strings.Contains(text, "another git process"):
		kind = ErrIndexLock
		message = "the Git index is locked by another process"
	case strings.Contains(text, "please tell me who you are") || strings.Contains(text, "author identity unknown") || strings.Contains(text, "committer identity unknown") || strings.Contains(text, "unable to auto-detect email") || strings.Contains(text, "empty ident name"):
		kind = ErrMissingIdentity
		message = "configure Git user.name and user.email before committing"
	case strings.Contains(text, "gpg failed") || strings.Contains(text, "failed to sign") || strings.Contains(text, "signing failed"):
		kind = ErrSigning
		message = "Git could not sign the commit non-interactively"
	case strings.Contains(text, "hook") || strings.Contains(text, "pre-commit") || strings.Contains(text, "commit-msg"):
		kind = ErrHook
		message = "a Git hook rejected the commit"
	}
	return &Error{Kind: kind, Message: message, Output: commandOutput(err), Cause: err}
}
func commandOutput(err error) string {
	var ce *commandError
	if errors.As(err, &ce) {
		return strings.TrimSpace(ce.stderr + "\n" + ce.stdout)
	}
	return ""
}

// runCommit is deliberately not tied to ctx after the process starts. Callers may
// abandon a request, but the host must drain Git to a known result.
func (r *Repository) runCommit(ctx context.Context, messageFile string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	args := []string{"-C", r.root, "commit", "--cleanup=verbatim", "--file=" + messageFile}
	cmd := exec.Command("git", args...)
	cmd.Env = gitEnvironment(nil)
	stdout := &limitBuffer{max: maxCommitOutput}
	stderr := &limitBuffer{max: maxCommitOutput}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", "", err
	}
	if err = cmd.Start(); err != nil {
		return "", "", err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(stdout, outPipe) }()
	go func() { defer wg.Done(); _, _ = io.Copy(stderr, errPipe) }()
	waitErr := cmd.Wait()
	wg.Wait()
	if stdout.truncated || stderr.truncated {
		if waitErr == nil {
			waitErr = &Error{Kind: ErrOutputLimit, Message: "Git commit output exceeded the display limit"}
		}
	}
	return stdout.String(), stderr.String(), waitErr
}

func removeFile(path string) { _ = os.Remove(path) }
func tempMessage(message string) (string, error) {
	f, err := os.CreateTemp("", "term-llm-commit-message-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if runtime.GOOS != "windows" {
		_ = f.Chmod(0600)
	}
	_, writeErr := io.WriteString(f, message)
	closeErr := f.Close()
	if writeErr != nil {
		removeFile(path)
		return "", writeErr
	}
	if closeErr != nil {
		removeFile(path)
		return "", closeErr
	}
	return path, nil
}
