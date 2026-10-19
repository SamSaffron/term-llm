//go:build !windows && !plan9 && !js && !wasip1

package shell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type unixProcess struct {
	file      *os.File
	cmd       *exec.Cmd
	done      chan Exit
	stopped   chan struct{}
	closeOnce sync.Once
}

func Supported() bool { return true }

func startProcess(cwd string, cols, rows int, output func([]byte)) (Process, error) {
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
	process := &unixProcess{file: file, cmd: cmd, done: make(chan Exit, 1), stopped: make(chan struct{})}
	readDone := make(chan struct{})
	outputChunks := make(chan []byte, 64)
	go func() {
		defer close(outputChunks)
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := file.Read(buffer)
			if n > 0 {
				outputChunks <- append([]byte(nil), buffer[:n]...)
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() {
		defer close(readDone)
		for chunk := range outputChunks {
			// Keep draining the PTY while a temporarily slow consumer handles the
			// previous callback. Coalescing queued reads both avoids backpressure
			// on short-lived producers and preserves output ordering.
			for {
				select {
				case next, ok := <-outputChunks:
					if !ok {
						output(chunk)
						return
					}
					chunk = append(chunk, next...)
				default:
					output(chunk)
					chunk = nil
				}
				if chunk == nil {
					break
				}
			}
		}
	}()
	go func() {
		waitErr := cmd.Wait()
		// A shell can exit while descendants retain the PTY. Its process group is
		// owned by this explicit user shell, so terminate any survivors, including
		// job-control groups in the same login session where the OS exposes them.
		_ = signalSession(cmd.Process.Pid, syscall.SIGKILL)
		_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-readDone:
		case <-time.After(drainWait):
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
		process.done <- Exit{Code: code, Err: waitErr}
		close(process.done)
		close(process.stopped)
	}()
	return process, nil
}

func signalGroup(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (p *unixProcess) Write(data []byte) (int, error) {
	return p.WriteContext(context.Background(), data)
}

func (p *unixProcess) WriteContext(ctx context.Context, data []byte) (int, error) {
	type writeResult struct {
		n   int
		err error
	}
	done := make(chan writeResult, 1)
	go func() {
		// Darwin interactive PTYs can duplicate input during large multiline
		// burst pastes. Feed physical lines with a short drain interval once a
		// paste exceeds 1KiB. Keep browser keystrokes and short protocol writes
		// immediate, and retain the
		// existing context-cancellation behavior for blocked or partial writes.
		pacedPaste := runtime.GOOS == "darwin" && len(data) > 1024
		total := 0
		for total < len(data) {
			end := len(data)
			if pacedPaste {
				if newline := bytes.IndexByte(data[total:], '\n'); newline >= 0 {
					end = total + newline + 1
				}
			}
			n, err := p.file.Write(data[total:end])
			total += n
			if n == 0 && err == nil {
				err = io.ErrShortWrite
			}
			if err != nil {
				done <- writeResult{n: total, err: err}
				return
			}
			if total < len(data) && pacedPaste {
				time.Sleep(5 * time.Millisecond)
			}
		}
		done <- writeResult{n: total}
	}()
	select {
	case result := <-done:
		return result.n, result.err
	case <-ctx.Done():
		// Closing the PTY master is the only portable way to unblock a write to a
		// canonical terminal whose input queue is full. A bounded write failure
		// therefore invalidates this generation rather than allowing later bytes to
		// race a still-running writer.
		_ = p.file.Close()
		_ = signalSession(p.cmd.Process.Pid, syscall.SIGHUP)
		_ = signalGroup(p.cmd.Process.Pid, syscall.SIGHUP)
		return 0, ctx.Err()
	}
}

func (p *unixProcess) Resize(cols, rows int) error {
	return pty.Setsize(p.file, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *unixProcess) Done() <-chan Exit { return p.done }

func (p *unixProcess) Close() {
	p.closeOnce.Do(func() {
		select {
		case <-p.stopped:
			return
		default:
		}
		_ = signalSession(p.cmd.Process.Pid, syscall.SIGHUP)
		_ = signalGroup(p.cmd.Process.Pid, syscall.SIGHUP)
		_ = signalSession(p.cmd.Process.Pid, syscall.SIGTERM)
		_ = signalGroup(p.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-p.stopped:
			return
		case <-time.After(750 * time.Millisecond):
		}
		_ = signalSession(p.cmd.Process.Pid, syscall.SIGKILL)
		_ = signalGroup(p.cmd.Process.Pid, syscall.SIGKILL)
		_ = p.file.Close()
		select {
		case <-p.stopped:
		case <-time.After(750 * time.Millisecond):
		}
	})
}
