//go:build !windows

package contain

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"
)

// runClaudeSetupTokenInPTY runs `claude setup-token` attached to a pseudo-TTY
// when the caller is truly interactive. Claude Code renders its login animation
// with terminal cursor controls only when stdout is a TTY; piping it through
// exec.Cmd's default copy path makes each frame print one after another. The
// PTY keeps the child in interactive terminal mode while still letting us tee
// the bytes into a buffer for token extraction.
func runClaudeSetupTokenInPTY(stdin io.Reader, stdout io.Writer) (string, bool, error) {
	stdinFile, stdinOK := stdin.(*os.File)
	stdoutFile, stdoutOK := stdout.(*os.File)
	if !stdinOK || !stdoutOK {
		return "", false, nil
	}
	if !term.IsTerminal(int(stdinFile.Fd())) || !term.IsTerminal(int(stdoutFile.Fd())) {
		return "", false, nil
	}

	cmd := exec.Command("claude", "setup-token")

	size, _ := pty.GetsizeFull(stdoutFile)
	ptmx, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return "", true, err
	}
	defer ptmx.Close()

	if size == nil {
		_ = pty.InheritSize(stdoutFile, ptmx)
	}
	resizeCh := make(chan os.Signal, 1)
	resizeDone := make(chan struct{})
	signal.Notify(resizeCh, syscall.SIGWINCH)
	defer signal.Stop(resizeCh)
	defer close(resizeDone)
	go func() {
		for {
			select {
			case <-resizeCh:
				_ = pty.InheritSize(stdoutFile, ptmx)
			case <-resizeDone:
				return
			}
		}
	}()
	resizeCh <- syscall.SIGWINCH

	restore := func() {}
	if oldState, err := term.MakeRaw(int(stdinFile.Fd())); err == nil {
		restore = func() { _ = term.Restore(int(stdinFile.Fd()), oldState) }
	}
	defer restore()

	var buf bytes.Buffer
	outputDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.MultiWriter(stdout, &buf), ptmx)
		outputDone <- err
	}()
	cancelableStdin, cancelErr := cancelreader.NewReader(stdinFile)
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		if cancelErr == nil {
			_, _ = io.Copy(ptmx, cancelableStdin)
			return
		}
		_, _ = io.Copy(ptmx, stdinFile)
	}()

	waitErr := cmd.Wait()
	_ = ptmx.Close()
	if cancelErr == nil {
		if cancelableStdin.Cancel() {
			<-stdinDone
		}
		_ = cancelableStdin.Close()
	}
	copyErr := <-outputDone
	if waitErr != nil {
		return buf.String(), true, waitErr
	}
	if copyErr != nil && !isExpectedPTYCloseError(copyErr) {
		return buf.String(), true, copyErr
	}
	return buf.String(), true, nil
}

func isExpectedPTYCloseError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EBADF) {
		return true
	}
	return false
}
