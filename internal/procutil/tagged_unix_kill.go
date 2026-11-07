//go:build !windows && !js

package procutil

import "syscall"

func syscallKill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
