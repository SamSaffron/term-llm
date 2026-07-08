package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ShellTool implements the shell tool.
type ShellTool struct {
	approval  *ApprovalManager
	config    *ToolConfig
	limits    OutputLimits
	shellPath string
	recorder  FileChangeRecorder
}

func shellApprovalTranscriptFromContext(ctx context.Context) []TranscriptEntry {
	msgs := llm.ApprovalTranscriptFromContext(ctx)
	if len(msgs) == 0 {
		return nil
	}
	entries := make([]TranscriptEntry, 0, len(msgs))
	for _, msg := range msgs {
		text := strings.TrimSpace(renderApprovalMessageText(msg))
		if text == "" {
			continue
		}
		role := strings.TrimSpace(msg.ApprovalRole)
		if role == "" {
			role = string(msg.Role)
		}
		entries = append(entries, TranscriptEntry{Role: role, Text: text})
	}
	return entries
}

func renderApprovalMessageText(msg llm.Message) string {
	var b strings.Builder
	for _, part := range msg.Parts {
		switch part.Type {
		case llm.PartText, llm.PartFile:
			if strings.TrimSpace(part.Text) != "" {
				appendApprovalSection(&b, part.Text)
			}
		case llm.PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			appendApprovalSection(&b, renderToolCallForApproval(part.ToolCall))
		case llm.PartToolResult:
			if part.ToolResult == nil {
				continue
			}
			appendApprovalSection(&b, renderToolResultForApproval(part.ToolResult))
		}
	}
	if b.Len() == 0 {
		return llm.MessageAttachmentSummary(msg)
	}
	return b.String()
}

func appendApprovalSection(b *strings.Builder, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(text)
}

func renderToolCallForApproval(call *llm.ToolCall) string {
	args := strings.TrimSpace(string(call.Arguments))
	if args == "" {
		args = "{}"
	}
	var pretty bytes.Buffer
	if json.Valid([]byte(args)) && json.Indent(&pretty, []byte(args), "", "  ") == nil {
		args = pretty.String()
	}
	return fmt.Sprintf("tool_call name=%q id=%q arguments:\n%s", call.Name, call.ID, args)
}

func renderToolResultForApproval(result *llm.ToolResult) string {
	status := "ok"
	if result.IsError {
		status = "error"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tool_result name=%q id=%q status=%s", result.Name, result.ID, status)
	if strings.TrimSpace(result.Content) != "" {
		fmt.Fprintf(&b, "\ncontent:\n%s", strings.TrimSpace(result.Content))
	}
	for _, part := range result.ContentParts {
		if part.Type == llm.ToolContentPartText && strings.TrimSpace(part.Text) != "" {
			fmt.Fprintf(&b, "\ncontent_part_text:\n%s", strings.TrimSpace(part.Text))
		}
		if part.Type == llm.ToolContentPartImageData && part.ImageData != nil {
			fmt.Fprintf(&b, "\ncontent_part_image: media_type=%q detail=%q bytes=%d", part.ImageData.MediaType, part.ImageData.Detail, len(part.ImageData.Base64))
		}
	}
	if len(result.Diffs) > 0 {
		fmt.Fprintf(&b, "\ndiffs: %d file diff(s)", len(result.Diffs))
	}
	if len(result.Images) > 0 {
		fmt.Fprintf(&b, "\nimages: %d image path(s)", len(result.Images))
	}
	return b.String()
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
	Command        string   `json:"command"`
	WorkingDir     string   `json:"working_dir,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Env            EnvMap   `json:"env,omitempty"`
	Description    string   `json:"description,omitempty"`
	AffectedPaths  []string `json:"affected_paths,omitempty"`
}

// ShellResult contains the result of a shell command.
type ShellResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
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
				"affected_paths": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional files or glob patterns (relative to working_dir, or absolute) this command may create, modify, or delete. Always declare them when running scripts or commands that change files: without this hint, change tracking is best-effort (git status and previously tracked files only) and changes may be missed.",
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
	a.Command, _ = extractLeadingCd(a.Command, a.WorkingDir)
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
	warning := WarnUnknownParams(args, []string{"command", "working_dir", "timeout_seconds", "description", "env", "affected_paths"})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}
	errorOutput := func(message string) llm.ToolOutput {
		output := textOutput(message)
		output.IsError = true
		return output
	}

	var a ShellArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errorOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Command == "" {
		return errorOutput(formatToolError(NewToolError(ErrInvalidParams, "command is required"))), nil
	}

	// Determine the default work dir before approval so prompts/project approvals
	// are scoped to the same directory exec.Cmd.Dir will use. Precedence:
	// explicit working_dir (resolved against BaseDir), then ShellWorkingDir, then
	// BaseDir, then process cwd.
	workDir := ""
	if a.WorkingDir != "" {
		if t.config != nil {
			workDir = t.config.ResolveDir(a.WorkingDir)
		} else {
			workDir = resolvePathAgainstBase(a.WorkingDir, "")
		}
	} else if t.config != nil {
		workDir = t.config.ShellDir()
	} else {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return errorOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot get working directory: %v", err))), nil
		}
	}

	// Strip leading "cd <dir> && " and fold into WorkingDir so that
	// the approval prompt shows only the real command, not the cd prefix.
	a.Command, workDir = extractLeadingCd(a.Command, workDir)

	// Check permissions — pass both command and working directory so the
	// approval UI can show the user where the command will run.
	if t.approval != nil {
		outcome, err := t.approval.CheckShellApprovalWithContext(ctx, a.Command, workDir, shellApprovalTranscriptFromContext(ctx))
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return errorOutput(formatToolError(toolErr)), nil
			}
			return errorOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return errorOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "command not allowed: %s", truncateCommand(a.Command)))), nil
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

	// workDir was resolved before approval and is the exact exec.Cmd.Dir below.

	// Validate working directory exists and is a directory
	if info, err := os.Stat(workDir); err != nil {
		if os.IsNotExist(err) {
			return errorOutput(formatToolError(NewToolErrorf(ErrExecutionFailed,
				"working directory %q does not exist", workDir))), nil
		}
		return errorOutput(formatToolError(NewToolErrorf(ErrExecutionFailed,
			"working directory %q is not accessible: %v", workDir, err))), nil
	} else if !info.IsDir() {
		return errorOutput(formatToolError(NewToolErrorf(ErrExecutionFailed,
			"working directory %q is not a directory", workDir))), nil
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

	cleanup, prepErr := prepareToolCommand(cmd)
	if prepErr != nil {
		return errorOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "command setup error: %v", prepErr))), nil
	}
	defer cleanup()

	stdout := newLimitedBuffer(t.limits.MaxBytes)
	stderr := newLimitedBuffer(t.limits.MaxBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Snapshot relevant files so changes made by the command can be recorded.
	snap := preShellSnapshot(ctx, t.recorder, workDir, a.AffectedPaths)

	// Run command
	err := cmd.Run()

	// Diff against the snapshot even on timeout or failure — partial writes
	// are real changes.
	fileChanges := postShellChanges(ctx, t.recorder, snap)

	result := ShellResult{
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		ExitCode:        0,
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}

	// Check for timeout
	if execCtx.Err() != nil {
		result.TimedOut = true
		return llm.ToolOutput{Content: warning + formatShellResult(result, t.limits), TimedOut: true, IsError: true, FileChanges: fileChanges}, nil
	}

	// Get exit code
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			output := textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "command error: %v", err)))
			output.IsError = true
			output.FileChanges = fileChanges
			return output, nil
		}
	}

	output := textOutput(formatShellResult(result, t.limits))
	output.IsError = result.ExitCode != 0
	output.FileChanges = fileChanges
	return output, nil
}

// formatShellResult formats the shell result for the LLM.
func formatShellResult(result ShellResult, limits OutputLimits) string {
	var sb strings.Builder

	// Truncate output if needed
	stdout := result.Stdout
	stderr := result.Stderr
	truncated := false

	if result.StdoutTruncated || int64(len(stdout)) > limits.MaxBytes {
		stdout = stdout[:limits.MaxBytes]
		truncated = true
	}
	if result.StderrTruncated || int64(len(stderr)) > limits.MaxBytes {
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

// expandTilde resolves a tilde prefix in a path.
// "~" or "~/sub" uses os.UserHomeDir; "~user" or "~user/sub" uses user.Lookup.
// Returns ("", false) if expansion fails.
func expandTilde(path string) (string, bool) {
	if path == "" || path[0] != '~' {
		return path, true
	}

	// Split into tilde component and optional rest after first separator.
	rest := ""
	slash := strings.IndexAny(path, string([]byte{'/', filepath.Separator}))
	tildePrefix := path
	if slash > 0 {
		tildePrefix = path[:slash]
		rest = path[slash:] // keeps the leading separator
	}

	var home string
	if tildePrefix == "~" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		home = h
	} else {
		username := tildePrefix[1:]
		u, err := user.Lookup(username)
		if err != nil {
			return "", false
		}
		home = u.HomeDir
	}
	return home + rest, true
}

// extractLeadingCd strips a leading "cd <dir> && " from a shell command and
// folds the directory into workDir. If the pattern is not matched or the path
// cannot be resolved, the original command and workDir are returned unchanged.
//
// Conservative by design: only rewrites plain literal paths whose meaning can
// be modelled exactly. No escape-sequence handling inside quotes, and only the
// "&&" separator is recognised (not ";"). Tilde expansion is only performed on
// unquoted paths (in POSIX shell, cd "~/x" does NOT expand the tilde).
func extractLeadingCd(command, workDir string) (string, string) {
	trimmed := strings.TrimSpace(command)
	if !strings.HasPrefix(trimmed, "cd ") && !strings.HasPrefix(trimmed, "cd\t") {
		return command, workDir
	}

	after := strings.TrimLeft(trimmed[2:], " \t") // skip "cd" + whitespace

	// Parse the path — track whether it was quoted so we can avoid
	// expanding shell constructs that quoting would suppress.
	var path, rest string
	quoted := false
	switch {
	case strings.HasPrefix(after, "'"):
		end := strings.Index(after[1:], "'")
		if end < 0 {
			return command, workDir
		}
		path = after[1 : end+1]
		rest = strings.TrimLeft(after[end+2:], " \t")
		quoted = true

	case strings.HasPrefix(after, "\""):
		end := strings.Index(after[1:], "\"")
		if end < 0 {
			return command, workDir
		}
		path = after[1 : end+1]
		rest = strings.TrimLeft(after[end+2:], " \t")
		quoted = true

	default:
		// Unquoted: path extends to next whitespace.
		idx := strings.IndexAny(after, " \t")
		if idx < 0 {
			return command, workDir // bare "cd path" with no continuation
		}
		path = after[:idx]
		rest = strings.TrimLeft(after[idx:], " \t")
	}

	// Must be followed by "&&" and a non-empty command.
	if !strings.HasPrefix(rest, "&&") {
		return command, workDir
	}
	remaining := strings.TrimLeft(rest[2:], " \t")
	if remaining == "" {
		return command, workDir
	}

	// Bail on shell-special cd operands we cannot model.
	if path == "-" || path == "~+" || path == "~-" {
		return command, workDir
	}

	// Bail on env-var, backtick, or backslash-escape expansion.
	if strings.ContainsAny(path, "$`\\") {
		return command, workDir
	}

	// Tilde expansion — only for unquoted paths. In POSIX shell,
	// cd "~/foo" and cd '~' treat ~ as a literal character.
	if !quoted && strings.HasPrefix(path, "~") {
		expanded, ok := expandTilde(path)
		if !ok {
			return command, workDir
		}
		path = expanded
	} else if strings.HasPrefix(path, "~") {
		// Quoted tilde — can't resolve without shell, bail.
		return command, workDir
	}

	// Resolve relative paths against workDir (or cwd).
	if !filepath.IsAbs(path) {
		base := workDir
		if base == "" {
			var err error
			base, err = os.Getwd()
			if err != nil {
				return command, workDir
			}
		}
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)

	return remaining, path
}

// truncateCommand truncates a command for error messages.
func truncateCommand(cmd string) string {
	if len(cmd) > 50 {
		return cmd[:47] + "..."
	}
	return cmd
}
