package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// RunAgentScriptTool executes scripts bundled in the agent's source directory.
// Scripts are resolved by filename only (no paths), and execution is implicitly
// trusted — no approval prompts are required.
type RunAgentScriptTool struct {
	config *ToolConfig
	limits OutputLimits
}

// NewRunAgentScriptTool creates a new RunAgentScriptTool.
func NewRunAgentScriptTool(config *ToolConfig, limits OutputLimits) *RunAgentScriptTool {
	return &RunAgentScriptTool{
		config: config,
		limits: limits,
	}
}

// RunAgentScriptArgs are the arguments for the run_agent_script tool.
type RunAgentScriptArgs struct {
	Script         string `json:"script"`
	Args           string `json:"args,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

func (t *RunAgentScriptTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        RunAgentScriptToolName,
		Description: "Execute a script bundled with the current agent. Scripts are referenced by filename only (e.g. \"create.sh\"), not by path.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"script": map[string]interface{}{
					"type":        "string",
					"description": "Script filename (e.g. \"create.sh\"). Must not contain path separators.",
				},
				"args": map[string]interface{}{
					"type":        "string",
					"description": "Arguments to pass to the script",
				},
				"timeout_seconds": map[string]interface{}{
					"type":        "integer",
					"description": "Script timeout in seconds (default: 30, max: 300)",
					"default":     30,
				},
			},
			"required":             []string{"script"},
			"additionalProperties": false,
		},
	}
}

func (t *RunAgentScriptTool) Preview(args json.RawMessage) string {
	var a RunAgentScriptArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Script == "" {
		return ""
	}
	preview := a.Script
	if a.Args != "" {
		preview += " " + a.Args
	}
	if len(preview) > 50 {
		preview = preview[:47] + "..."
	}
	return preview
}

func (t *RunAgentScriptTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a RunAgentScriptArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Script == "" {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "script is required"))), nil
	}

	// Validate AgentDir is configured
	if t.config.AgentDir == "" {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "no agent directory configured"))), nil
	}

	// Security: reject path separators and traversal
	if strings.Contains(a.Script, "/") || strings.Contains(a.Script, "\\") || strings.Contains(a.Script, "..") {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "script name must not contain path separators or '..'"))), nil
	}

	// Resolve absolute path and verify containment within AgentDir
	absScript := filepath.Join(t.config.AgentDir, a.Script)
	absScript, err := filepath.Abs(absScript)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "resolve path: %v", err))), nil
	}

	agentDir, err := filepath.Abs(t.config.AgentDir)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "resolve agent dir: %v", err))), nil
	}

	if !strings.HasPrefix(absScript, agentDir+string(filepath.Separator)) {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "script path escapes agent directory"))), nil
	}

	// Resolve symlinks and re-check containment
	realScript, err := filepath.EvalSymlinks(absScript)
	if err != nil {
		if os.IsNotExist(err) {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrFileNotFound, "script not found: %s", a.Script))), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "resolve symlinks: %v", err))), nil
	}

	realAgentDir, err := filepath.EvalSymlinks(agentDir)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "resolve agent dir symlinks: %v", err))), nil
	}

	if !strings.HasPrefix(realScript, realAgentDir+string(filepath.Separator)) {
		return llm.TextOutput(formatToolError(NewToolError(ErrSymlinkEscape, "script symlink escapes agent directory"))), nil
	}

	// Verify target is a file
	info, err := os.Stat(realScript)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrFileNotFound, "script not found: %s", a.Script))), nil
	}
	if info.IsDir() {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "script target is a directory, not a file"))), nil
	}

	// Set timeout
	timeout := 30
	if a.TimeoutSeconds > 0 {
		timeout = a.TimeoutSeconds
	}
	if timeout > 300 {
		timeout = 300
	}

	// Get working directory
	workDir, err := os.Getwd()
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot get working directory: %v", err))), nil
	}

	// Build command
	shell := detectShell()
	cmdStr := realScript
	if a.Args != "" {
		cmdStr += " " + a.Args
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, shell, "-c", cmdStr)
	cmd.Dir = workDir

	// Isolate stdin: tools are non-interactive; never share the TUI's raw stdin
	// with child processes.
	devNull, openErr := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if openErr == nil {
		cmd.Stdin = devNull
		defer devNull.Close()
	}

	// Put child in its own process group so signals don't cross-contaminate
	// and exec.CommandContext can kill the whole group on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	execErr := cmd.Run()

	result := ShellResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}

	if execCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		return llm.TextOutput(formatShellResult(result, t.limits)), nil
	}

	if execErr != nil {
		if exitErr, ok := execErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "script error: %v", execErr))), nil
		}
	}

	return llm.TextOutput(formatShellResult(result, t.limits)), nil
}
