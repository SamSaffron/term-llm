//go:build !windows

package cmd

import (
	"os"
	"strings"
)

// execReload replaces the current process with a fresh instance of the same
// binary, resuming sessionID if non-empty.  It strips any pre-existing
// --resume / -r flags from os.Args and appends the new one so the flags don't
// stack across repeated reloads.
//
// On success this function never returns.  On failure it returns an error.
func execReload(sessionID string) error {
	// Re-exec'ing the SAME binary: hand back any env-provided hub delegation
	// and registration tokens that startup scrubbed from the environment, or the
	// next generation would silently lose hub access. This env goes only to
	// ourselves, never to tool subprocesses.
	return replaceProcess(reloadExecutable, chatReloadArgs(os.Args, sessionID), processReloadEnviron())
}

// --resume has NoOptDefVal: a bare --resume/-r never consumes the next token.
// Keep positional arguments after -- literal, and insert the resume flag before it.
func chatReloadArgs(args []string, sessionID string) []string {
	newArgs := []string{args[0]}
	var positional []string
	for i, arg := range args[1:] {
		if arg == "--" {
			positional = args[i+1:]
			break
		}
		if arg == "--resume" || arg == "-r" || strings.HasPrefix(arg, "--resume=") || strings.HasPrefix(arg, "-r=") {
			continue
		}
		newArgs = append(newArgs, arg)
	}
	if sessionID != "" {
		newArgs = append(newArgs, "--resume="+sessionID)
	}
	return append(newArgs, positional...)
}
