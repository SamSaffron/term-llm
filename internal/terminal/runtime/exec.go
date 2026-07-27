package tea

import (
	"io"
	"os"
	"os/exec"
)

type execMsg struct {
	cmd execCommand
	fn  func(error) Msg
}

func execCommandCmd(c execCommand, fn func(error) Msg) Cmd {
	return func() Msg { return execMsg{cmd: c, fn: fn} }
}

// ExecProcess runs the given *exec.Cmd in a blocking fashion, effectively
// pausing the Program while the command is running. After the command exits,
// the Program restores the terminal and resumes. It is useful for spawning
// interactive applications such as editors and shells from within a Program.
//
// The callback may return a message containing the command or terminal-restore
// error. If errors are irrelevant, fn may be nil. For non-interactive I/O, use
// a Cmd instead.
func ExecProcess(c *exec.Cmd, fn func(error) Msg) Cmd {
	return execCommandCmd(wrapExecCommand(c), fn)
}

// execCommand is the retained internal subset of upstream ExecCommand used by
// ExecProcess to run a command while the terminal is released.
type execCommand interface {
	Run() error
	SetStdin(io.Reader)
	SetStdout(io.Writer)
	SetStderr(io.Writer)
}

// wrapExecCommand wraps an exec.Cmd so it satisfies execCommand.
func wrapExecCommand(c *exec.Cmd) execCommand {
	return &osExecCommand{Cmd: c}
}

// osExecCommand adapts exec.Cmd to execCommand without replacing streams the
// caller already configured.
type osExecCommand struct{ *exec.Cmd }

// SetStdin sets stdin on the underlying exec.Cmd when it is unset.
func (c *osExecCommand) SetStdin(r io.Reader) {
	if c.Stdin == nil {
		c.Stdin = r
	}
}

// SetStdout sets stdout on the underlying exec.Cmd when it is unset.
func (c *osExecCommand) SetStdout(w io.Writer) {
	if c.Stdout == nil {
		c.Stdout = w
	}
}

// SetStderr sets stderr on the underlying exec.Cmd when it is unset.
func (c *osExecCommand) SetStderr(w io.Writer) {
	if c.Stderr == nil {
		c.Stderr = w
	}
}

// exec runs an execCommand while terminal ownership is released, restores
// Program ownership, and delivers the result through fn.
func (p *Program) exec(c execCommand, fn func(error) Msg) {
	if err := p.releaseTerminal(false); err != nil {
		if fn != nil {
			go p.Send(fn(err))
		}
		return
	}

	c.SetStdin(p.input)
	c.SetStdout(p.output)
	c.SetStderr(os.Stderr)

	if err := c.Run(); err != nil {
		_ = p.RestoreTerminal()
		if fn != nil {
			go p.Send(fn(err))
		}
		return
	}

	err := p.RestoreTerminal()
	if fn != nil {
		go p.Send(fn(err))
	}
}
