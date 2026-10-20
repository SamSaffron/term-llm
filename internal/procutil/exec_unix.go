//go:build !js && !wasip1

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ConfigureDetachedCommand starts cmd in a new session and configures
// process-group cancellation.
func ConfigureDetachedCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	configureProcessGroupCancellation(cmd)
}

// ConfigureCommandProcessGroup starts cmd in its own process group without
// removing access to the caller's controlling terminal.
func ConfigureCommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	configureProcessGroupCancellation(cmd)
}

func configureProcessGroupCancellation(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
			return nil
		}

		// The process group can disappear between context cancellation and the
		// signal on fast-exiting commands. Fall back to the direct child so
		// os/exec sees os.ErrProcessDone instead of surfacing a cancellation race.
		err := cmd.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
}
