//go:build windows

package widgets

import (
	"os"
	"os/exec"
	"syscall"
)

// killProcessGroup is the fallback for manually constructed widget entries used
// by tests. Production entries always hold an owned procutil command.
func killProcessGroup(proc *os.Process, _ *exec.Cmd, _ syscall.Signal) {
	if proc != nil {
		_ = proc.Kill()
	}
}
