package procutil

import (
	"bytes"
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

// commandMode selects the ownership model for a prepared command.
type commandMode int

const (
	// detachedMode starts a new session/process group and owns its descendants.
	detachedMode commandMode = iota
	// processGroupMode starts a new process group while retaining the caller's
	// controlling terminal (for interactive helpers).
	processGroupMode
)

type platformCommand interface {
	start(*Command) error
	signal(syscall.Signal) error
	cancel(*Command) error
	close(*Command) error
}

// ErrCommandClosed is returned by Start after Close has been called. Close
// before Start never creates an OS process.
var ErrCommandClosed = errors.New("procutil: command is closed")

var errCommandAlreadyStarted = errors.New("procutil: command already started")

// Command owns an exec.Cmd and its process tree. Set the command's I/O and
// environment before starting it, then call Close to release owned resources.
// Start and Wait may each be called once; Close is idempotent. A closed Command
// cannot be restarted.
type Command struct {
	cmd       *exec.Cmd
	platform  platformCommand
	mu        sync.Mutex
	started   bool
	closed    bool
	closeOnce sync.Once
	closeErr  error
	closeHook func()
}

// PrepareCommand configures a detached command with the platform ownership
// policy. The caller must Close the returned Command.
func PrepareCommand(cmd *exec.Cmd) (*Command, error) {
	return prepareCommand(cmd, detachedMode)
}

// PrepareCommandProcessGroup is PrepareCommand for interactive helpers that
// retain the caller's controlling terminal.
func PrepareCommandProcessGroup(cmd *exec.Cmd) (*Command, error) {
	return prepareCommand(cmd, processGroupMode)
}

func prepareCommand(cmd *exec.Cmd, mode commandMode) (*Command, error) {
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

// Start starts the prepared command. Close before Start is rejected before any
// OS process is created. A failure after process creation always terminates and
// reaps the direct process, even if the owned tree was closed concurrently.
func (c *Command) Start() error {
	if c == nil || c.cmd == nil || c.platform == nil {
		return errors.New("start command: nil command")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrCommandClosed
	}
	if c.started {
		c.mu.Unlock()
		return errCommandAlreadyStarted
	}
	c.started = true
	c.mu.Unlock()

	err := c.platform.start(c)
	if err == nil {
		return nil
	}
	// Release the platform tree first, then reap outside every platform lock so
	// a context-cancel callback cannot deadlock against this path.
	_ = c.Close()
	if c.cmd.Process != nil {
		_ = c.Wait()
	}
	return err
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
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
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

// Output runs a prepared command and captures stdout. Like exec.Cmd.Output, it
// rejects a preconfigured Stdout and preserves a preconfigured Stderr; Stderr is
// populated on ExitError only when it was previously nil.
func (c *Command) Output() ([]byte, error) {
	if c == nil || c.cmd == nil {
		return nil, errors.New("output command: nil command")
	}
	if c.cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var stdout bytes.Buffer
	c.cmd.Stdout = &stdout
	captureStderr := c.cmd.Stderr == nil
	var stderr bytes.Buffer
	if captureStderr {
		c.cmd.Stderr = &stderr
	}
	err := c.Run()
	if exitErr, ok := err.(*exec.ExitError); ok && captureStderr {
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

// cancel is the exec.Cmd context-cancellation hook. It differs from Close by
// preserving os.ErrProcessDone when the direct process already exited, so
// os/exec does not turn a successful exit into a cancellation error.
func (c *Command) cancel() error {
	if c == nil || c.platform == nil {
		return nil
	}
	return c.platform.cancel(c)
}

// Close terminates/releases the owned platform tree.
// It is idempotent and safe after Wait, before Start, or concurrently with
// context cancellation.
func (c *Command) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
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
