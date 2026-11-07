//go:build !js && !windows

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

type unixPlatformCommand struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	started bool
	closed  bool
}

func configurePlatformCommand(cmd *exec.Cmd, mode CommandMode, owner *Command) (platformCommand, error) {
	attr := &syscall.SysProcAttr{}
	if mode == DetachedMode {
		attr.Setsid = true
	} else {
		attr.Setpgid = true
	}
	cmd.SysProcAttr = attr
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return owner.Close() }
	}
	return &unixPlatformCommand{cmd: cmd}, nil
}

func (p *unixPlatformCommand) start(*Command) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrCommandClosed
	}
	if p.started {
		p.mu.Unlock()
		return errCommandAlreadyStarted
	}
	p.started = true
	err := p.cmd.Start()
	p.mu.Unlock()
	return err
}

func (p *unixPlatformCommand) signal(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-p.cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (p *unixPlatformCommand) close(*Command) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	pid := p.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		if err := p.cmd.Process.Kill(); !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	return nil
}
