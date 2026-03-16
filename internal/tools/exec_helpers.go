package tools

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/gobwas/glob"
	"github.com/samsaffron/term-llm/internal/pathutil"
)

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
	if cmd.Stdin == nil {
		devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
		if err == nil {
			cmd.Stdin = devNull
			cleanup := func() {
				_ = devNull.Close()
			}
			configureCommandProcessGroup(cmd)
			return cleanup, nil
		}
	}

	configureCommandProcessGroup(cmd)
	return func() {}, nil
}

func configureCommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
