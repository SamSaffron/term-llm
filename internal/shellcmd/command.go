package shellcmd

import (
	"context"
	"os/exec"
)

// CommandContext builds a shell command using the platform shell selected by
// Path. Callers may adjust fields such as Dir and Env before starting it.
func CommandContext(ctx context.Context, command string) *exec.Cmd {
	return CommandContextFor(ctx, Path(), command)
}

// CommandContextFor builds a shell command for an already resolved shell path.
// It keeps shell grammar and platform command-line construction in one place.
func CommandContextFor(ctx context.Context, shellPath, command string) *exec.Cmd {
	return CommandContextArgs(ctx, shellPath, Args(shellPath, command)...)
}

// CommandContextArgs builds a command from a resolved shell invocation. The
// platform implementation may attach a native command line for shells that do
// not use the normal Windows argv quoting rules.
func CommandContextArgs(ctx context.Context, shellPath string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, shellPath, args...)
	configureCommand(cmd, shellPath, args)
	return cmd
}

// Command builds a non-context shell command for interactive callers.
func Command(shellPath string, args ...string) *exec.Cmd {
	cmd := exec.Command(shellPath, args...)
	configureCommand(cmd, shellPath, args)
	return cmd
}
