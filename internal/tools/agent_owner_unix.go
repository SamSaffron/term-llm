//go:build !linux && !windows

package tools

import (
	"errors"
	"syscall"
)

// A missing PID proves termination; EPERM and all other errors do not.
func ownerPIDTerminated(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
