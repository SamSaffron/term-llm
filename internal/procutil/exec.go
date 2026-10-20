package procutil

import (
	"bytes"
	"os"
	"os/exec"
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

// PrepareCommand configures a captured subprocess in a detached session so it
// cannot bypass its configured streams through the caller's controlling TTY.
func PrepareCommand(cmd *exec.Cmd) (func(), error) {
	return prepareCommand(cmd, ConfigureDetachedCommand)
}

// PrepareCommandProcessGroup preserves controlling-terminal access while still
// providing process-group cancellation. Use this only for explicitly
// interactive helpers such as credential resolvers.
func PrepareCommandProcessGroup(cmd *exec.Cmd) (func(), error) {
	return prepareCommand(cmd, ConfigureCommandProcessGroup)
}

func prepareCommand(cmd *exec.Cmd, configure func(*exec.Cmd)) (func(), error) {
	if cmd.Stdin == nil {
		devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
		if err == nil {
			cmd.Stdin = devNull
			configure(cmd)
			return func() {
				_ = devNull.Close()
			}, nil
		}
	}

	configure(cmd)
	return func() {}, nil
}
