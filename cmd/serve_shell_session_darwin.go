//go:build darwin

package cmd

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// signalServeShellSession reaches background job-control groups by walking the
// process tree while the PTY shell is still alive. Darwin does not expose the
// Linux /proc session table, but kern.proc.all includes stable parent PIDs.
func signalServeShellSession(rootPID int, signal syscall.Signal) error {
	if rootPID <= 0 {
		return nil
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return err
	}
	children := make(map[int][]int)
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		parent := int(process.Eproc.Ppid)
		if pid > 0 && parent > 0 {
			children[parent] = append(children[parent], pid)
		}
	}

	var result error
	var signalDescendants func(int)
	signalDescendants = func(parent int) {
		for _, pid := range children[parent] {
			signalDescendants(pid)
			if killErr := syscall.Kill(pid, signal); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
				result = errors.Join(result, fmt.Errorf("signal shell descendant %d: %w", pid, killErr))
			}
		}
	}
	signalDescendants(rootPID)
	return result
}
