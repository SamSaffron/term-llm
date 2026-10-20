package main

import (
	"context"
	"errors"
)

var errShellUnavailable = errors.New("shell needs the browser page; no Linux guest is installed in this build")

// ShellExecutor is the page-owned Linux VM bridge. The parent agent owns the
// same browser tab's v86 guest behind
// globalThis.termLLMRunShell(requestJSON): Promise<string>.
type ShellExecutor interface {
	RunShell(ctx context.Context, requestJSON string) (string, error)
}

// unavailableShellExecutor keeps native builds and tests honest: without the
// page there is no guest, so the tool reports that instead of running anything
// on the host.
type unavailableShellExecutor struct{}

func (u *unavailableShellExecutor) RunShell(context.Context, string) (string, error) {
	return "", errShellUnavailable
}
