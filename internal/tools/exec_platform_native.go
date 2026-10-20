//go:build !js

package tools

import (
	"errors"
	"os/exec"
	"syscall"
)

func platformKillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func platformKillPid(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }

func isTextFileBusy(err error) bool { return errors.Is(err, syscall.ETXTBSY) }
