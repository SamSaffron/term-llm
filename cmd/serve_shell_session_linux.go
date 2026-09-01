//go:build linux

package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// signalServeShellSession reaches job-control groups that an interactive shell
// moved out of its own process group. Every process in the PTY login session is
// owned by this shell generation.
func signalServeShellSession(sessionID int, signal syscall.Signal) error {
	entries, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		data, readErr := os.ReadFile(entry)
		if readErr != nil {
			continue
		}
		line := string(data)
		closeParen := strings.LastIndexByte(line, ')')
		if closeParen < 0 || closeParen+2 >= len(line) {
			continue
		}
		fields := strings.Fields(line[closeParen+2:])
		// Fields after comm begin with state, ppid, pgrp, session.
		if len(fields) < 4 || fields[3] != strconv.Itoa(sessionID) {
			continue
		}
		pidText := filepath.Base(filepath.Dir(entry))
		pid, parseErr := strconv.Atoi(pidText)
		if parseErr != nil || pid <= 0 {
			continue
		}
		if killErr := syscall.Kill(pid, signal); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			result = errors.Join(result, fmt.Errorf("signal shell session process %d: %w", pid, killErr))
		}
	}
	return result
}
