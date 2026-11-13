package tools

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/pathutil"
	"github.com/samsaffron/term-llm/internal/procutil"
	"github.com/samsaffron/term-llm/internal/shellcmd"
)

// Keep existing script hooks source-compatible while sharing bounded capture.
type limitedBuffer = procutil.LimitedBuffer

func newLimitedBuffer(limit int64) *limitedBuffer {
	return procutil.NewLimitedBuffer(limit)
}

func prepareToolCommand(cmd *exec.Cmd) (*procutil.Command, error) {
	return procutil.PrepareToolCommand(cmd)
}

const scriptBusyMaxRetries = 4

// retryScriptBusy reports whether a failed script exec should be retried,
// sleeping briefly before returning true. ETXTBSY is transient: a fork from a
// concurrent goroutine can inherit the just-written script's writable file
// descriptor and hold it across our exec, e.g. when a script is written and
// immediately executed while other commands are being spawned.
func retryScriptBusy(ctx context.Context, err error, attempt int) bool {
	if attempt >= scriptBusyMaxRetries || ctx.Err() != nil || !isTextFileBusy(err) {
		return false
	}
	time.Sleep(10 * time.Millisecond)
	return true
}

// PrepareCommand configures a tool-owned command for safe, non-interactive
// execution. The child starts a new session so it cannot open the caller's
// controlling terminal, and cancellation tears down its process group.
func PrepareCommand(cmd *exec.Cmd) (*procutil.Command, error) {
	return prepareToolCommand(cmd)
}

func splitShellWords(input string) ([]string, error) {
	return shellcmd.SplitShellWords(input)
}

// SplitShellWords parses a command line into argv without invoking a shell.
func SplitShellWords(input string) ([]string, error) {
	return shellcmd.SplitShellWords(input)
}

func hasUnsafeShellSyntax(input string) bool { return shellcmd.HasUnsafeShellSyntax(input) }

// HasUnsafeShellSyntax reports whether input uses shell operators that require a shell.
func HasUnsafeShellSyntax(input string) bool { return shellcmd.HasUnsafeShellSyntax(input) }

func validateShellApprovalPattern(pattern string) error {
	return shellcmd.ValidateApprovalPattern(pattern)
}

func matchShellPattern(pattern, value string) bool { return shellcmd.MatchPattern(pattern, value) }

func resolveToolPath(path string, isWrite bool) (string, error) {
	return resolveToolPathInBase(path, isWrite, "")
}

func resolveToolPathWithConfig(path string, isWrite bool, config *ToolConfig) (string, error) {
	baseDir := ""
	if config != nil {
		baseDir = config.WorkingDir()
		if config.RequiresExplicitWorkingDir() && strings.TrimSpace(baseDir) == "" {
			expanded, err := pathutil.Expand(path)
			if err != nil {
				return "", NewToolErrorf(ErrInvalidParams, "%v", err)
			}
			if !filepath.IsAbs(expanded) {
				return "", NewToolError(ErrInvalidParams, "relative path requires an absolute path or explicit session working directory")
			}
		}
	}
	return resolveToolPathInBase(path, isWrite, baseDir)
}

func resolveToolPathInBase(path string, isWrite bool, baseDir string) (string, error) {
	expanded, err := pathutil.Expand(path)
	if err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "%v", err)
	}
	if strings.TrimSpace(expanded) == "" {
		return "", NewToolError(ErrInvalidParams, "path is required")
	}
	if !filepath.IsAbs(expanded) && strings.TrimSpace(baseDir) != "" {
		expanded = filepath.Join(baseDir, expanded)
	}
	if isWrite {
		return canonicalizePathForWrite(expanded)
	}
	return canonicalizePath(expanded)
}
