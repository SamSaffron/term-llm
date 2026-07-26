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

// ExecProcess runs c while Bubble Tea releases the terminal, then restores the
// terminal and reports the result through fn.
func ExecProcess(c *exec.Cmd, fn func(error) Msg) Cmd {
	return execCommandCmd(wrapExecCommand(c), fn)
}

type execCommand interface {
	Run() error
	SetStdin(io.Reader)
	SetStdout(io.Writer)
	SetStderr(io.Writer)
}

func wrapExecCommand(c *exec.Cmd) execCommand {
	return &osExecCommand{Cmd: c}
}

type osExecCommand struct{ *exec.Cmd }

func (c *osExecCommand) SetStdin(r io.Reader) {
	if c.Stdin == nil {
		c.Stdin = r
	}
}

func (c *osExecCommand) SetStdout(w io.Writer) {
	if c.Stdout == nil {
		c.Stdout = w
	}
}

func (c *osExecCommand) SetStderr(w io.Writer) {
	if c.Stderr == nil {
		c.Stderr = w
	}
}

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
