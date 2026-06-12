//go:build !windows

package cmd

import (
	"os"
	"strings"
	"syscall"

	"github.com/samsaffron/term-llm/internal/tools"
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

	// Re-exec'ing the SAME binary: hand back any env-provided hub delegation
	// token that startup scrubbed from the environment, or the next
	// generation would silently lose hub access. This env goes only to
	// ourselves, never to tool subprocesses.
	return syscall.Exec(exe, newArgs, append(os.Environ(), tools.HubDelegationEnviron()...))
}
