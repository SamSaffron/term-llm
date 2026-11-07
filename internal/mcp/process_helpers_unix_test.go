//go:build !windows

package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func killProcessIfRunning(pid int) {
	if pid <= 0 || mcpProcessHasExited(pid) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func mcpProcessHasExited(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS == "linux" {
		state, ok := linuxMCPProcState(pid)
		return ok && state == 'Z'
	}
	return false
}

func linuxMCPProcState(pid int) (byte, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return mcpProcStatState(data)
}

func mcpProcStatState(data []byte) (byte, bool) {
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end == -1 || end+2 >= len(stat) {
		return 0, false
	}
	return stat[end+2], true
}
