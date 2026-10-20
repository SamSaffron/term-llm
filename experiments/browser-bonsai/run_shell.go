package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	// The guest mounts the shared workspace at /workspace. Host paths are never
	// sent to the page, and working_dir is always normalized into this tree.
	shellGuestRoot = "/workspace"

	defaultShellTimeoutSeconds = 10
	maxShellTimeoutSeconds     = 30

	// The VM boots lazily on the first command. That boot is additional to the
	// command's execution budget, so the bridge waits well past timeout_seconds.
	shellBootGrace = 180 * time.Second

	maxShellFiles      = 256
	maxShellFileBytes  = 8 << 20
	maxShellStdoutSize = 64 << 10
	maxShellStderrSize = 64 << 10
)

// shellArgs is the model-facing schema. working_dir and timeout_seconds are
// optional; working_dir is always interpreted inside /workspace.
type shellArgs struct {
	Command        string `json:"command"`
	WorkingDir     string `json:"working_dir"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// shellRequest is the frozen Go -> page request shape.
type shellRequest struct {
	Command        string            `json:"command"`
	WorkingDir     string            `json:"working_dir"`
	Files          map[string]string `json:"files"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// shellResult is the frozen page -> Go result shape. Files is the FULL
// post-execution workspace map, which Go diffs against its own snapshot.
type shellResult struct {
	Stdout   string            `json:"stdout"`
	Stderr   string            `json:"stderr"`
	ExitCode int               `json:"exit_code"`
	Files    map[string]string `json:"files"`
}

// shellTool runs commands in the page's real Linux guest (BusyBox + CPython 3.12)
// over the same in-memory workspace the real file tools use. It never invokes a
// host shell.
type shellTool struct {
	app *App
}

func (t *shellTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name: tools.ShellToolName,
		Description: "Run a command in a real Linux guest (v86) with BusyBox (sh, awk, sed, grep, coreutils) " +
			"and CPython 3.12 (python/python3) inside an in-browser virtual machine over the current workspace. " +
			"The tested Python standard-library set is json, csv, statistics, unittest, and sqlite3; " +
			"base64, binascii, gzip, and zipfile work through bundled zlib. " +
			"Optional extension modules needing unbundled libraries (ssl, ctypes, bz2, lzma, readline) are unavailable. " +
			"There is no pip, package installation, or network; node and Go are not installed. " +
			"This is a real Linux guest, not the host shell. " +
			"working_dir is a directory inside /workspace (default /workspace). The first command may need to boot the " +
			"VM and install the Python runtime, so it can take longer; timeout_seconds bounds command execution " +
			"(default 10, maximum 30). stdout, stderr, the exit code, and resulting workspace files are returned.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "Shell command line to run in the Linux guest.",
				},
				"working_dir": map[string]interface{}{
					"type":        "string",
					"description": "Directory inside /workspace, for example /workspace/playground (default /workspace).",
				},
				"timeout_seconds": map[string]interface{}{
					"type":        "integer",
					"description": "Command execution timeout in seconds (default 10, maximum 30).",
					"minimum":     1,
					"maximum":     maxShellTimeoutSeconds,
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

func (t *shellTool) Preview(args json.RawMessage) string {
	var a shellArgs
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	command := strings.TrimSpace(a.Command)
	if command == "" {
		return ""
	}
	if index := strings.IndexByte(command, '\n'); index >= 0 {
		command = command[:index]
	}
	if len(command) > 100 {
		command = command[:100] + "…"
	}
	if dir := strings.TrimSpace(a.WorkingDir); dir != "" {
		return tools.ShellToolName + ": " + command + " (" + dir + ")"
	}
	return tools.ShellToolName + ": " + command
}

func (t *shellTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	start := time.Now()
	parsed, warning, err := parseShellArgs(args)
	if err != nil {
		return shellFailure(tools.ErrInvalidParams, err.Error(), warning), nil
	}

	workingDir, err := normalizeShellWorkingDir(t.app.workspace, parsed.WorkingDir)
	if err != nil {
		return shellFailure(tools.ErrInvalidParams, err.Error(), warning), nil
	}

	snapshot, err := snapshotWorkspaceFiles(t.app.workspace)
	if err != nil {
		return shellFailure(tools.ErrExecutionFailed, err.Error(), warning), nil
	}
	if err := checkShellSnapshot(snapshot); err != nil {
		return shellFailure(tools.ErrFileTooLarge, err.Error(), warning), nil
	}

	request, err := json.Marshal(shellRequest{
		Command:        parsed.Command,
		WorkingDir:     workingDir,
		Files:          snapshot,
		TimeoutSeconds: parsed.TimeoutSeconds,
	})
	if err != nil {
		return shellFailure(tools.ErrExecutionFailed, "encode guest request: "+err.Error(), warning), nil
	}

	executor := t.app.shellExecutor()
	if executor == nil {
		return shellFailure(tools.ErrExecutionFailed, "no shell executor is configured", warning), nil
	}
	guardCtx, cancel := context.WithTimeout(ctx, time.Duration(parsed.TimeoutSeconds)*time.Second+shellBootGrace)
	defer cancel()

	rawResult, err := executor.RunShell(guardCtx, string(request))
	duration := time.Since(start)
	if err != nil {
		output := shellFailure(tools.ErrExecutionFailed, "guest: "+err.Error(), warning)
		output.Content += fmt.Sprintf("\nduration_ms: %d", duration.Milliseconds())
		switch {
		case guardCtx.Err() == context.DeadlineExceeded:
			output.Content = warning + fmt.Sprintf("Error [%s]: shell timed out after %d seconds\nduration_ms: %d",
				tools.ErrTimeout, parsed.TimeoutSeconds, duration.Milliseconds())
			output.TimedOut = true
		case ctx.Err() != nil:
			output.Content = warning + fmt.Sprintf("Error [%s]: shell command cancelled\nduration_ms: %d",
				tools.ErrCancelledByUser, duration.Milliseconds())
		}
		return output, nil
	}

	var result shellResult
	if err := json.Unmarshal([]byte(rawResult), &result); err != nil {
		return shellFailure(tools.ErrExecutionFailed, "decode guest result JSON: "+err.Error(), warning), nil
	}
	if result.Files == nil {
		return shellFailure(tools.ErrExecutionFailed,
			"guest result is missing `files`; the full post-execution workspace map is required", warning), nil
	}
	files, err := validateReturnedShellFiles(result.Files)
	if err != nil {
		return shellFailure(tools.ErrInvalidParams, "rejected guest file map: "+err.Error(), warning), nil
	}

	written, deleted, unchanged, err := applyShellWorkspace(t.app.workspace, snapshot, files)
	duration = time.Since(start)
	content := formatShellResult(result, workingDir, duration, written, deleted, unchanged)
	if err != nil {
		content += "\nworkspace apply error: " + err.Error()
	}
	if warning != "" {
		content = warning + content
	}
	output := core.TextOutput(content)
	output.IsError = result.ExitCode != 0 || err != nil
	return output, nil
}

// parseShellArgs validates the model-facing arguments before touching the VM.
func parseShellArgs(args json.RawMessage) (shellArgs, string, error) {
	var warning string
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return shellArgs{}, "", fmt.Errorf("invalid arguments: %w", err)
	}
	allowed := map[string]bool{"command": true, "working_dir": true, "timeout_seconds": true}
	var unknown []string
	for key := range raw {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		warning = fmt.Sprintf("warning: ignored unknown parameters: %s\n", strings.Join(unknown, ", "))
	}

	var parsed shellArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return shellArgs{}, warning, fmt.Errorf("invalid arguments: %w", err)
	}
	parsed.Command = strings.TrimSpace(parsed.Command)
	parsed.WorkingDir = strings.TrimSpace(parsed.WorkingDir)
	if parsed.Command == "" {
		return shellArgs{}, warning, fmt.Errorf("command is required")
	}
	if parsed.TimeoutSeconds == 0 {
		parsed.TimeoutSeconds = defaultShellTimeoutSeconds
	}
	if parsed.TimeoutSeconds < 1 || parsed.TimeoutSeconds > maxShellTimeoutSeconds {
		return shellArgs{}, warning, fmt.Errorf("timeout_seconds must be between 1 and %d (got %d)",
			maxShellTimeoutSeconds, parsed.TimeoutSeconds)
	}
	return parsed, warning, nil
}

// normalizeShellWorkingDir maps a model-supplied directory to the guest's
// /workspace tree. Guest paths are accepted directly; a host workspace path
// (native tests) is stripped to its relative form. Nothing outside the
// workspace can become a guest working directory.
func normalizeShellWorkingDir(root, workingDir string) (string, error) {
	workingDir = strings.TrimSpace(workingDir)
	if workingDir == "" {
		return shellGuestRoot, nil
	}
	candidate := filepath.ToSlash(workingDir)
	if candidate == "." {
		return shellGuestRoot, nil
	}
	if len(candidate) > 1 {
		candidate = strings.TrimSuffix(candidate, "/")
	}
	if candidate == shellGuestRoot {
		return shellGuestRoot, nil
	}
	if strings.HasPrefix(candidate, shellGuestRoot+"/") {
		rel := strings.TrimPrefix(candidate, shellGuestRoot+"/")
		clean, err := cleanWorkspaceRelative(rel)
		if err != nil {
			return "", err
		}
		return shellGuestRoot + "/" + clean, nil
	}
	if candidate == filepath.ToSlash(root) {
		return shellGuestRoot, nil
	}
	rel, err := workspaceRelativePath(root, candidate)
	if err != nil {
		return "", err
	}
	return shellGuestRoot + "/" + rel, nil
}

// checkShellSnapshot enforces the outbound workspace budget.
func checkShellSnapshot(files map[string]string) error {
	if len(files) > maxShellFiles {
		return fmt.Errorf("workspace has %d text files; the guest accepts at most %d", len(files), maxShellFiles)
	}
	total := 0
	for _, content := range files {
		total += len(content)
	}
	if total > maxShellFileBytes {
		return fmt.Errorf("workspace text is %d bytes; the guest accepts at most %d", total, maxShellFileBytes)
	}
	return nil
}

// validateReturnedShellFiles rejects the whole map before any change is applied
// if a single path is unsafe or the result is unbounded.
func validateReturnedShellFiles(files map[string]string) (map[string]string, error) {
	if len(files) > maxShellFiles {
		return nil, fmt.Errorf("guest returned %d files; the limit is %d", len(files), maxShellFiles)
	}
	validated := make(map[string]string, len(files))
	total := 0
	for name, content := range files {
		rel, err := cleanWorkspaceRelative(name)
		if err != nil {
			return nil, err
		}
		total += len(content)
		if total > maxShellFileBytes {
			return nil, fmt.Errorf("guest returned more than %d bytes of file content", maxShellFileBytes)
		}
		validated[rel] = content
	}
	return validated, nil
}

// applyShellWorkspace writes changed/new files and deletes files the guest
// dropped. Writes happen first so a failed delete cannot lose content.
func applyShellWorkspace(root string, before, after map[string]string) (written, deleted, unchanged int, err error) {
	for _, rel := range sortedKeys(after) {
		content := after[rel]
		if previous, ok := before[rel]; ok && previous == content {
			unchanged++
			continue
		}
		if writeErr := writeWorkspaceFile(root, rel, content); writeErr != nil {
			return written, deleted, unchanged, fmt.Errorf("write %s: %w", rel, writeErr)
		}
		written++
	}
	for _, rel := range sortedKeys(before) {
		if _, ok := after[rel]; ok {
			continue
		}
		if removeErr := removeWorkspaceFile(root, rel); removeErr != nil {
			return written, deleted, unchanged, fmt.Errorf("delete %s: %w", rel, removeErr)
		}
		deleted++
	}
	return written, deleted, unchanged, nil
}

func formatShellResult(result shellResult, workingDir string, duration time.Duration, written, deleted, unchanged int) string {
	var out strings.Builder
	fmt.Fprintf(&out, "exit_code: %d\n", result.ExitCode)
	fmt.Fprintf(&out, "duration_ms: %d\n", duration.Milliseconds())
	fmt.Fprintf(&out, "working_dir: %s\n", workingDir)
	fmt.Fprintf(&out, "workspace: %d written, %d deleted, %d unchanged\n", written, deleted, unchanged)
	out.WriteString("stdout:\n")
	out.WriteString(formatShellStream(result.Stdout, maxShellStdoutSize))
	out.WriteString("\nstderr:\n")
	out.WriteString(formatShellStream(result.Stderr, maxShellStderrSize))
	return out.String()
}

func formatShellStream(text string, limit int) string {
	if text == "" {
		return "(empty)"
	}
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("\n... [truncated %d bytes]", len(text)-len(cut))
}

func sortedKeys(files map[string]string) []string {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func shellFailure(errType tools.ToolErrorType, message, warning string) core.ToolOutput {
	return core.ToolOutput{
		Content: warning + fmt.Sprintf("Error [%s]: %s", errType, message),
		IsError: true,
	}
}
