package procutil

import (
	"bytes"
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

// CommandMode selects the ownership model for a prepared command.
type CommandMode int

const (
	// DetachedMode starts a new session/process group and owns its descendants.
	DetachedMode CommandMode = iota
	// ProcessGroupMode starts a new process group while retaining the caller's
	// controlling terminal (for interactive helpers).
	ProcessGroupMode
)

// Command wraps exec.Cmd with an explicit platform ownership lifetime. Construct
// one after setting fields such as Dir/Env/Stdout/Stderr, then use
// Start/Run/Wait and always Close it to release the platform tree.
// The raw Cmd remains available for field setup and ProcessState inspection.
type platformCommand interface {
	start(*Command) error
	signal(syscall.Signal) error
	close(*Command) error
}

type Command struct {
	cmd       *exec.Cmd
	platform  platformCommand
	closeOnce sync.Once
	closeErr  error
	closeHook func()
}

// PrepareCommand configures a detached command with the platform ownership
// policy. The caller must Close the returned Command.
func PrepareCommand(cmd *exec.Cmd) (*Command, error) {
	return prepareCommand(cmd, DetachedMode)
}

// PrepareCommandProcessGroup is PrepareCommand for interactive helpers that
// retain the caller's controlling terminal.
func PrepareCommandProcessGroup(cmd *exec.Cmd) (*Command, error) {
	return prepareCommand(cmd, ProcessGroupMode)
}

func prepareCommand(cmd *exec.Cmd, mode CommandMode) (*Command, error) {
	if cmd == nil {
		return nil, errors.New("prepare command: nil command")
	}
	c := &Command{cmd: cmd}
	platform, err := configurePlatformCommand(cmd, mode, c)
	if err != nil {
		return nil, err
	}
	c.platform = platform
	return c, nil
}

// Cmd exposes the underlying exec.Cmd for fields not owned by this wrapper.
func (c *Command) Cmd() *exec.Cmd {
	if c == nil {
		return nil
	}
	return c.cmd
}

// Start starts the prepared command. On failure it releases the platform tree.
func (c *Command) Start() error {
	if c == nil || c.cmd == nil {
		return errors.New("start command: nil command")
	}
	if err := c.platform.start(c); err != nil {
		_ = c.Close()
		return err
	}
	return nil
}

// Run starts and waits for the command. Close remains the caller's
// responsibility for reaping descendants and releasing platform resources.
func (c *Command) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Signal delivers sig to the owned process group where the platform supports
// it. Windows maps this to tree termination through Close.
func (c *Command) Signal(sig syscall.Signal) error {
	if c == nil || c.platform == nil {
		return nil
	}
	return c.platform.signal(sig)
}

// Wait waits for the direct command process.
func (c *Command) Wait() error {
	if c == nil || c.cmd == nil {
		return errors.New("wait command: nil command")
	}
	return c.cmd.Wait()
}

// Output runs a prepared command and captures stdout/stderr, preserving
// exec.ExitError.Stderr semantics.
func (c *Command) Output() ([]byte, error) {
	if c == nil || c.cmd == nil {
		return nil, errors.New("output command: nil command")
	}
	var stdout, stderr bytes.Buffer
	c.cmd.Stdout = &stdout
	c.cmd.Stderr = &stderr
	err := c.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitErr.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), err
}

// SetCloseHook registers an additional cleanup (for example tagged-descendant
// reaping) to run once when Close releases the command. It must be set before
// concurrent Start/Close use.
func (c *Command) SetCloseHook(fn func()) {
	if c != nil {
		c.closeHook = fn
	}
}

// Close terminates/releases the owned platform tree.
// It is idempotent and safe after Wait, before Start, or concurrently with
// context cancellation.
func (c *Command) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.platform != nil {
			c.closeErr = c.platform.close(c)
		}
		if c.closeHook != nil {
			c.closeHook()
		}
	})
	return c.closeErr
}
