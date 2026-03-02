//go:build !windows

package cmd

import (
	"os"
	"strings"
	"syscall"
)

// execReload replaces the current process with a fresh instance of the same
// binary, resuming sessionID if non-empty.  It strips any pre-existing
// --resume / -r flags from os.Args and appends the new one so the flags don't
// stack across repeated reloads.
//
// On success this function never returns.  On failure it returns an error.
func execReload(sessionID string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	// Rebuild argv, stripping any existing --resume / -r flags.
	newArgs := []string{os.Args[0]}
	skipNext := false
	for _, arg := range os.Args[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--resume" || arg == "-r" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--resume=") || strings.HasPrefix(arg, "-r=") {
			continue
		}
		newArgs = append(newArgs, arg)
	}

	if sessionID != "" {
		// Use --resume=ID (not --resume ID) because the flag has NoOptDefVal set,
		// which means cobra treats the next positional arg as a separate argument
		// rather than the flag value when the two-arg form is used.
		newArgs = append(newArgs, "--resume="+sessionID)
	}

	return syscall.Exec(exe, newArgs, os.Environ())
}
