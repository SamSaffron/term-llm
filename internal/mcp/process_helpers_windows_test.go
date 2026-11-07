//go:build windows

package mcp

import (
	"os/exec"
	"strconv"

	"golang.org/x/sys/windows"
)

func killProcessIfRunning(pid int) {
	if pid <= 0 || mcpProcessHasExited(pid) {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

func mcpProcessHasExited(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code != 259
}
