package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ShellTool implements the shell tool.
type ShellTool struct {
	approval  *ApprovalManager
	config    *ToolConfig
	limits    OutputLimits
	shellPath string
}

// NewShellTool creates a new ShellTool.
func NewShellTool(approval *ApprovalManager, config *ToolConfig, limits OutputLimits) *ShellTool {
	return &ShellTool{
		approval:  approval,
		config:    config,
		limits:    limits,
		shellPath: detectShell(),
	}
}

// EnvMap is a string-to-string map that can unmarshal both the standard JSON
// object form ({"KEY":"val"}) used by non-strict providers, and the array
// form ([{"key":"KEY","value":"val"}]) emitted by OpenAI strict-mode schemas
// where additionalProperties must be false.
type EnvMap map[string]string

// UnmarshalJSON implements json.Unmarshaler.
func (e *EnvMap) UnmarshalJSON(data []byte) error {
	// Try array of key/value pairs first (Responses API strict-mode form).
	var pairs []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &pairs); err == nil {
		m := make(map[string]string, len(pairs))
		for _, p := range pairs {
			if p.Key == "" {
				return fmt.Errorf("env pair has empty key")
			}
			m[p.Key] = p.Value
		}
		*e = m
		return nil
	}
	// Fall back to plain map (Chat Completions / non-strict form).
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*e = m
	return nil
}

// ShellArgs are the arguments for the shell tool.
type ShellArgs struct {
	Command        string `json:"command"`
	WorkingDir     string `json:"working_dir,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Env            EnvMap `json:"env,omitempty"`
	Description    string `json:"description,omitempty"`
}

// ShellResult contains the result of a shell command.
type ShellResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

func (t *ShellTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        ShellToolName,
		Description: "Execute a shell command. Returns stdout, stderr, and exit code.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "Shell command to execute",
				},
				"working_dir": map[string]interface{}{
					"type":        "string",
					"description": "Working directory (defaults to current directory)",
				},
				"timeout_seconds": map[string]interface{}{
					"type":        "integer",
					"description": "Command timeout in seconds (default: 30, max: 300)",
					"default":     30,
				},
				"env": map[string]interface{}{
					"type":                 "object",
					"description":          "Environment variables to set for the command",
					"additionalProperties": map[string]interface{}{"type": "string"},
				},
				"description": map[string]interface{}{
					"type":        "string",
					"description": "Optional short human-readable label (≤10 words) describing what this command does",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

func (t *ShellTool) Preview(args json.RawMessage) string {
	var a ShellArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Command == "" {
		return ""
	}
	if a.Description != "" {
		desc := a.Description
		runes := []rune(desc)
		if len(runes) > 100 {
			desc = string(runes[:97]) + "..."
		}
		return desc
	}
	cmd := a.Command
	if len(cmd) > 50 {
		cmd = cmd[:47] + "..."
	}
	return cmd
}

func (t *ShellTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	warning := WarnUnknownParams(args, []string{"command", "working_dir", "timeout_seconds", "description", "env"})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}

	var a ShellArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Command == "" {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "command is required"))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckShellApproval(a.Command)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return textOutput(formatToolError(toolErr)), nil
			}
			return textOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return textOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "command not allowed: %s", truncateCommand(a.Command)))), nil
		}
	}

	// Set timeout
	timeout := 30
	if a.TimeoutSeconds > 0 {
		timeout = a.TimeoutSeconds
	}
	if timeout > 300 {
		timeout = 300
	}

	// Set working directory
	workDir := a.WorkingDir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot get working directory: %v", err))), nil
		}
	}

	// Create command with context timeout
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, t.shellPath, "-c", a.Command)
	cmd.Dir = workDir
	overrides := make(map[string]struct{}, len(a.Env))
	for key := range a.Env {
		overrides[key] = struct{}{}
	}
	cmd.Env = make([]string, 0, len(os.Environ())+len(a.Env))
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok {
			if _, shadowed := overrides[k]; shadowed {
				continue
			}
		}
		cmd.Env = append(cmd.Env, e)
	}
	for key, value := range a.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
	}

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

	// Run command
	err := cmd.Run()

	result := ShellResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}

	// Check for timeout
	if execCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		return llm.ToolOutput{Content: warning + formatShellResult(result, t.limits), TimedOut: true}, nil
	}

	// Get exit code
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "command error: %v", err))), nil
		}
	}

	return textOutput(formatShellResult(result, t.limits)), nil
}

// formatShellResult formats the shell result for the LLM.
func formatShellResult(result ShellResult, limits OutputLimits) string {
	var sb strings.Builder

	// Truncate output if needed
	stdout := result.Stdout
	stderr := result.Stderr
	truncated := false

	if int64(len(stdout)) > limits.MaxBytes {
		stdout = stdout[:limits.MaxBytes]
		truncated = true
	}
	if int64(len(stderr)) > limits.MaxBytes {
		stderr = stderr[:limits.MaxBytes]
		truncated = true
	}

	if result.TimedOut {
		sb.WriteString("[Command timed out]\n\n")
	}

	if stdout != "" {
		sb.WriteString("stdout:\n")
		sb.WriteString(stdout)
		if !strings.HasSuffix(stdout, "\n") {
			sb.WriteString("\n")
		}
	}

	if stderr != "" {
		if stdout != "" {
			sb.WriteString("\n")
		}
		sb.WriteString("stderr:\n")
		sb.WriteString(stderr)
		if !strings.HasSuffix(stderr, "\n") {
			sb.WriteString("\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\nexit_code: %d", result.ExitCode))

	if truncated {
		sb.WriteString("\n\n[Output truncated due to size limit]")
	}

	return sb.String()
}

// detectShell returns the user's shell.
func detectShell() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return "bash"
	}
	// Use full path for execution
	return shell
}

// truncateCommand truncates a command for error messages.
func truncateCommand(cmd string) string {
	if len(cmd) > 50 {
		return cmd[:47] + "..."
	}
	return cmd
}
