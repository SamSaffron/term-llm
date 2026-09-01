//go:build !windows && !plan9 && !js && !wasip1

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type unixServeShellProcess struct {
	file      *os.File
	cmd       *exec.Cmd
	done      chan serveShellExit
	stopped   chan struct{}
	closeOnce sync.Once
}

func platformServeShellSupported() bool { return true }

func startServeShellProcess(cwd string, cols, rows int, output func([]byte)) (serveShellProcess, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor", "TERM_PROGRAM=term-llm")
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	process := &unixServeShellProcess{file: file, cmd: cmd, done: make(chan serveShellExit, 1), stopped: make(chan struct{})}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := file.Read(buffer)
			if n > 0 {
				output(append([]byte(nil), buffer[:n]...))
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() {
		waitErr := cmd.Wait()
		// A shell can exit while descendants retain the PTY. Its process group is
		// owned by this explicit user shell, so terminate any survivors, including
		// job-control groups in the same login session where the OS exposes them.
		_ = signalServeShellSession(cmd.Process.Pid, syscall.SIGKILL)
		_ = signalServeShellGroup(cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-readDone:
		case <-time.After(serveShellDrainWait):
			// The slave side should close after every session process is gone. Keep
			// shutdown bounded if a platform or driver fails to deliver EOF.
			_ = file.Close()
			<-readDone
		}
		_ = file.Close()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		} else if waitErr != nil {
			code = -1
		}
		process.done <- serveShellExit{Code: code, Err: waitErr}
		close(process.done)
		close(process.stopped)
	}()
	return process, nil
}

func signalServeShellGroup(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (p *unixServeShellProcess) Write(data []byte) (int, error) { return p.file.Write(data) }

func (p *unixServeShellProcess) Resize(cols, rows int) error {
	return pty.Setsize(p.file, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *unixServeShellProcess) Done() <-chan serveShellExit { return p.done }

func (p *unixServeShellProcess) Close() {
	p.closeOnce.Do(func() {
		select {
		case <-p.stopped:
			return
		default:
		}
		_ = signalServeShellSession(p.cmd.Process.Pid, syscall.SIGHUP)
		_ = signalServeShellGroup(p.cmd.Process.Pid, syscall.SIGHUP)
		_ = signalServeShellSession(p.cmd.Process.Pid, syscall.SIGTERM)
		_ = signalServeShellGroup(p.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-p.stopped:
			return
		case <-time.After(750 * time.Millisecond):
		}
		_ = signalServeShellSession(p.cmd.Process.Pid, syscall.SIGKILL)
		_ = signalServeShellGroup(p.cmd.Process.Pid, syscall.SIGKILL)
		_ = p.file.Close()
		select {
		case <-p.stopped:
		case <-time.After(750 * time.Millisecond):
		}
	})
}
