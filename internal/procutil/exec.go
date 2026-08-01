package procutil

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type LimitedBuffer struct {
	buf   bytes.Buffer
	limit int64
	total int64
}

func NewLimitedBuffer(limit int64) *LimitedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &LimitedBuffer{limit: limit}
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
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

func (b *LimitedBuffer) String() string {
	return b.buf.String()
}

func (b *LimitedBuffer) Truncated() bool {
	return b.total > int64(b.buf.Len())
}

func PrepareCommand(cmd *exec.Cmd) (func(), error) {
	if cmd.Stdin == nil {
		devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
		if err == nil {
			cmd.Stdin = devNull
			ConfigureCommandProcessGroup(cmd)
			return func() {
				_ = devNull.Close()
			}, nil
		}
	}

	ConfigureCommandProcessGroup(cmd)
	return func() {}, nil
}

func ConfigureCommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
			return nil
		}

		// The process group can disappear between context cancellation and the
		// signal on fast-exiting commands. Fall back to the direct child so
		// os/exec sees os.ErrProcessDone instead of surfacing a cancellation race.
		err := cmd.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
}
