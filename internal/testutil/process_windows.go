//go:build windows

package testutil

import "golang.org/x/sys/windows"

const stillActive = 259

// ProcessHasExited reports whether pid refers to a process that is no longer
// running. Windows has no zombies, so an exited process is reported as exited
// even while handles to it remain open.
func ProcessHasExited(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code != stillActive
}
