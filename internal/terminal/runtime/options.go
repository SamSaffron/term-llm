package tea

import (
	"context"
	"io"
)

// ProgramOption is used to set options when initializing a Program. Program can
// accept a variable number of options.
//
// Example usage:
//
//	p := NewProgram(model, WithInput(someInput), WithOutput(someOutput))
type ProgramOption func(*Program)

// WithContext lets you specify a context in which to run the Program. This is
// useful if you want to cancel the execution from outside. When a Program gets
// cancelled it will exit with an error ErrProgramKilled.
func WithContext(ctx context.Context) ProgramOption {
	return func(p *Program) {
		p.externalCtx = ctx
	}
}

// WithOutput sets the output which, by default, is stdout. In most cases you
// won't need to use this.
func WithOutput(output io.Writer) ProgramOption {
	return func(p *Program) {
		p.output = output
	}
}

// WithInput sets the input which, by default, is stdin. In most cases you
// won't need to use this. To disable input entirely pass nil.
//
//	p := NewProgram(model, WithInput(nil))
func WithInput(input io.Reader) ProgramOption {
	return func(p *Program) {
		p.input = input
		p.disableInput = input == nil
	}
}

// WithEnvironment sets the environment variables that the program will use.
// This useful when the program is running in a remote session (e.g. SSH) and
// you want to pass the environment variables from the remote session to the
// program.
//
// Example:
//
//	var sess ssh.Session // ssh.Session is a type from the github.com/charmbracelet/ssh package
//	pty, _, _ := sess.Pty()
//	environ := append(sess.Environ(), "TERM="+pty.Term)
//	p := tea.NewProgram(model, tea.WithEnvironment(environ)
func WithEnvironment(env []string) ProgramOption {
	return func(p *Program) {
		p.environ = env
	}
}

// WithoutSignalHandler disables the signal handler that Bubble Tea sets up for
// Programs. This is useful if you want to handle signals yourself.
func WithoutSignalHandler() ProgramOption {
	return func(p *Program) {
		p.disableSignalHandler = true
	}
}

// WithFPS sets a custom maximum FPS at which the renderer should run. If
// less than 1, the default value of 60 will be used. If over 120, the FPS
// will be capped at 120.
func WithFPS(fps int) ProgramOption {
	return func(p *Program) {
		p.fps = fps
	}
}

// WithWindowSize sets the initial size of the terminal window. This is useful
// when you need to set the initial size of the terminal window, for example
// during testing or when you want to run your program in a non-interactive
// environment.
func WithWindowSize(width, height int) ProgramOption {
	return func(p *Program) {
		p.width = width
		p.height = height
	}
}
