package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/samsaffron/term-llm/internal/pathutil"
	"github.com/samsaffron/term-llm/internal/procutil"
)

// shellNonceEnvVar is the environment variable name used to tag every process
// spawned by a tool command so descendants that escape the process group (via
// setsid, daemonisation, etc.) can still be reliably reaped on cleanup.
const shellNonceEnvVar = "TERMLLM_SHELL_NONCE"

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int64
	total int64
}

func newLimitedBuffer(limit int64) *limitedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	b.total += int64(origLen)

	remaining := b.limit - int64(b.buf.Len())
	if remaining <= 0 {
		return origLen, nil
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	_, _ = b.buf.Write(p)
	return origLen, nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func (b *limitedBuffer) Truncated() bool {
	return b.total > int64(b.buf.Len())
}

func prepareToolCommand(cmd *exec.Cmd) (*procutil.Command, error) {
	owned, err := procutil.PrepareCommand(cmd)
	if err != nil {
		return nil, err
	}
	configureToolWait(cmd)
	nonce := tagCommandWithNonce(cmd)
	owned.SetCloseHook(func() {
		// Tools must leave the world clean: after the command returns (success,
		// failure, timeout, or cancellation) reap every descendant it spawned.
		// The owner has already killed the process group; this second pass walks
		// /proc for descendants that escaped it via setsid/daemonisation.
		procutil.KillTaggedDescendants(shellNonceEnvVar, nonce)
	})
	return owned, nil
}

const scriptBusyMaxRetries = 4

// retryScriptBusy reports whether a failed script exec should be retried,
// sleeping briefly before returning true. ETXTBSY is transient: a fork from a
// concurrent goroutine can inherit the just-written script's writable file
// descriptor and hold it across our exec, e.g. when a script is written and
// immediately executed while other commands are being spawned.
func retryScriptBusy(ctx context.Context, err error, attempt int) bool {
	if attempt >= scriptBusyMaxRetries || ctx.Err() != nil || !isTextFileBusy(err) {
		return false
	}
	time.Sleep(10 * time.Millisecond)
	return true
}

// tagCommandWithNonce adds a unique TERMLLM_SHELL_NONCE=<hex> entry to cmd.Env
// so descendants can be identified later by scanning /proc. Returns the nonce
// value, or "" if it could not be generated (in which case tagging is silently
// skipped — the pgroup kill still runs).
func tagCommandWithNonce(cmd *exec.Cmd) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	nonce := hex.EncodeToString(buf[:])
	entry := shellNonceEnvVar + "=" + nonce
	if cmd.Env == nil {
		cmd.Env = append(os.Environ(), entry)
	} else {
		cmd.Env = append(cmd.Env, entry)
	}
	return nonce
}

// PrepareCommand configures a tool-owned command for safe, non-interactive
// execution. The child starts a new session so it cannot open the caller's
// controlling terminal, and cancellation tears down its process group.
func PrepareCommand(cmd *exec.Cmd) (*procutil.Command, error) {
	return prepareToolCommand(cmd)
}

func configureToolWait(cmd *exec.Cmd) {
	// WaitDelay unblocks cmd.Wait() when the main process has exited but a
	// backgrounded descendant still holds the stdout/stderr pipe open. After
	// the delay Go closes the pipes and returns from Wait, preventing the
	// tool from hanging indefinitely.
	cmd.WaitDelay = 2 * time.Second
}

func splitShellWords(input string) ([]string, error) {
	var (
		args         []string
		current      strings.Builder
		inSingle     bool
		inDouble     bool
		escaped      bool
		tokenStarted bool
	)

	flush := func() {
		if tokenStarted {
			args = append(args, current.String())
			current.Reset()
			tokenStarted = false
		}
	}

	for _, r := range input {
		switch {
		case escaped:
			current.WriteRune(r)
			tokenStarted = true
			escaped = false

		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				current.WriteRune(r)
				tokenStarted = true
			}

		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			default:
				current.WriteRune(r)
				tokenStarted = true
			}

		default:
			switch r {
			case '\'':
				inSingle = true
				tokenStarted = true
			case '"':
				inDouble = true
				tokenStarted = true
			case '\\':
				escaped = true
				tokenStarted = true
			case ' ', '\t', '\n', '\r':
				flush()
			default:
				current.WriteRune(r)
				tokenStarted = true
			}
		}
	}

	if escaped || inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quoted argument")
	}
	flush()
	return args, nil
}

// SplitShellWords parses a command line into argv without invoking a shell.
func SplitShellWords(input string) ([]string, error) {
	return splitShellWords(input)
}

func hasUnsafeShellSyntax(input string) bool {
	inSingle := false
	inDouble := false
	escaped := false

	for _, r := range input {
		switch {
		case escaped:
			escaped = false
			continue

		case inSingle:
			if r == '\'' {
				inSingle = false
			}
			continue

		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			case '$', '`':
				return true
			}
			continue

		default:
			switch r {
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case '\\':
				escaped = true
			case ';', '|', '&', '<', '>', '(', ')', '\n', '\r', '$', '`':
				return true
			}
		}
	}

	return false
}

// HasUnsafeShellSyntax reports whether input uses shell operators that require a shell.
func HasUnsafeShellSyntax(input string) bool {
	return hasUnsafeShellSyntax(input)
}

func validateShellApprovalPattern(pattern string) error {
	parts, err := splitShellWords(pattern)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("shell pattern is required")
	}
	for _, part := range parts {
		if _, err := doublestar.Match(part, ""); err != nil {
			return fmt.Errorf("invalid glob in shell word %q: %w", part, err)
		}
	}
	return nil
}

func matchShellPattern(pattern, value string) bool {
	matched, err := doublestar.Match(pattern, value)
	return err == nil && matched
}

func resolveToolPath(path string, isWrite bool) (string, error) {
	return resolveToolPathInBase(path, isWrite, "")
}

func resolveToolPathWithConfig(path string, isWrite bool, config *ToolConfig) (string, error) {
	baseDir := ""
	if config != nil {
		baseDir = config.WorkingDir()
		if config.RequiresExplicitWorkingDir() && strings.TrimSpace(baseDir) == "" {
			expanded, err := pathutil.Expand(path)
			if err != nil {
				return "", NewToolErrorf(ErrInvalidParams, "%v", err)
			}
			if !filepath.IsAbs(expanded) {
				return "", NewToolError(ErrInvalidParams, "relative path requires an absolute path or explicit session working directory")
			}
		}
	}
	return resolveToolPathInBase(path, isWrite, baseDir)
}

func resolveToolPathInBase(path string, isWrite bool, baseDir string) (string, error) {
	expanded, err := pathutil.Expand(path)
	if err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "%v", err)
	}
	if strings.TrimSpace(expanded) == "" {
		return "", NewToolError(ErrInvalidParams, "path is required")
	}
	if !filepath.IsAbs(expanded) && strings.TrimSpace(baseDir) != "" {
		expanded = filepath.Join(baseDir, expanded)
	}
	if isWrite {
		return canonicalizePathForWrite(expanded)
	}
	return canonicalizePath(expanded)
}
