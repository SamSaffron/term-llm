package tools

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gobwas/glob"
	"github.com/samsaffron/term-llm/internal/pathutil"
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

func prepareToolCommand(cmd *exec.Cmd) (func(), error) {
	var stdinCloser func()
	if cmd.Stdin == nil {
		if devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0); err == nil {
			cmd.Stdin = devNull
			stdinCloser = func() { _ = devNull.Close() }
		}
	}

	configureCommandProcessGroup(cmd)
	nonce := tagCommandWithNonce(cmd)

	cleanup := func() {
		// Tools must leave the world clean: after the command returns (success,
		// failure, timeout, or cancellation) reap every descendant it spawned.
		//
		// First pass: SIGKILL the process group so `nohup foo &` style children
		// that stayed in our pgid die immediately.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		// Second pass: walk /proc for any process still alive that inherited
		// the nonce env var. This catches descendants that escaped the pgroup
		// via setsid, double-fork daemonisation, or explicit setpgid. We retry
		// a few times because a process may be mid-exec when we scan.
		if nonce != "" {
			killTaggedDescendants(nonce)
		}
		if stdinCloser != nil {
			stdinCloser()
		}
	}
	return cleanup, nil
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

// killTaggedDescendants SIGKILLs every process whose environment contains the
// given nonce. Safe no-op on systems without /proc (e.g. macOS in tests).
// We retry a handful of times with short sleeps to handle races where a
// descendant forks right as we scan.
func killTaggedDescendants(nonce string) {
	needle := []byte(shellNonceEnvVar + "=" + nonce)
	for attempt := 0; attempt < 3; attempt++ {
		pids := findPidsWithEnv(needle)
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// findPidsWithEnv returns PIDs whose /proc/<pid>/environ contains needle.
// On systems without /proc, returns nil.
func findPidsWithEnv(needle []byte) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	selfPid := os.Getpid()
	var pids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == selfPid {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		if bytes.Contains(data, needle) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// PrepareCommand configures a command for safe cancellation, including process-group teardown.
func PrepareCommand(cmd *exec.Cmd) (func(), error) {
	return prepareToolCommand(cmd)
}

func configureCommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// WaitDelay unblocks cmd.Wait() when the main process has exited but a
	// backgrounded descendant still holds the stdout/stderr pipe open. After
	// the delay Go closes the pipes and returns from Wait, preventing the
	// tool from hanging indefinitely.
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
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

func matchShellPattern(pattern, value string) bool {
	g, err := glob.Compile(pattern)
	if err != nil {
		return false
	}
	return g.Match(value)
}

func resolveToolPath(path string, isWrite bool) (string, error) {
	expanded, err := pathutil.Expand(path)
	if err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "%v", err)
	}
	if isWrite {
		return canonicalizePathForWrite(expanded)
	}
	return canonicalizePath(expanded)
}
