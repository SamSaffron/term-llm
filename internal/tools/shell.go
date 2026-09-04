package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ShellTool implements the shell tool.
type ShellTool struct {
	approval  *ApprovalManager
	config    *ToolConfig
	limits    OutputLimits
	shellPath string

	recorderMu sync.RWMutex
	recorder   FileChangeRecorder

	collaborationMu sync.RWMutex
	controller      CollaborativeShellController
	routingMode     ShellRoutingMode
}

func (t *ShellTool) fileChangeRecorder() FileChangeRecorder {
	t.recorderMu.RLock()
	defer t.recorderMu.RUnlock()
	return t.recorder
}

func (t *ShellTool) setFileChangeRecorder(recorder FileChangeRecorder) {
	t.recorderMu.Lock()
	t.recorder = recorder
	t.recorderMu.Unlock()
}

func (t *ShellTool) setCollaborativeShell(controller CollaborativeShellController, mode ShellRoutingMode) {
	t.collaborationMu.Lock()
	t.controller = controller
	t.routingMode = mode
	t.collaborationMu.Unlock()
}

func (t *ShellTool) collaborativeShell() (CollaborativeShellController, ShellRoutingMode) {
	t.collaborationMu.RLock()
	defer t.collaborationMu.RUnlock()
	return t.controller, t.routingMode
}

// PrepareRequestContext implements llm.RequestContextTool.
func (t *ShellTool) PrepareRequestContext(ctx context.Context, sessionID string, messages []llm.Message) ([]llm.Message, error) {
	controller, _ := t.collaborativeShell()
	if controller == nil {
		return messages, nil
	}
	return controller.PrepareRequestContext(ctx, sessionID, messages)
}

// PrepareCompactionContext implements llm.RequestContextTool.
func (t *ShellTool) PrepareCompactionContext(ctx context.Context, sessionID string, result *llm.CompactionResult) error {
	controller, _ := t.collaborativeShell()
	if controller == nil {
		return nil
	}
	return controller.PrepareCompactionContext(ctx, sessionID, result)
}

func approvalTranscriptFromContext(ctx context.Context) []TranscriptEntry {
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

// OutputClaim declares intended shell output before execution.
type OutputClaim struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// ShellArgs are the arguments for the shell tool.
type ShellArgs struct {
	Command        string        `json:"command"`
	WorkingDir     string        `json:"working_dir,omitempty"`
	TimeoutSeconds int           `json:"timeout_seconds,omitempty"`
	Env            EnvMap        `json:"env,omitempty"`
	Description    string        `json:"description,omitempty"`
	AffectedPaths  []string      `json:"affected_paths,omitempty"`
	OutputClaims   []OutputClaim `json:"output_claims,omitempty"`
}

// ShellResult contains the result of a shell command.
type ShellResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	Canceled        bool   `json:"canceled,omitempty"`
	RecoveryFailed  bool   `json:"recovery_failed,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

func (t *ShellTool) Spec() llm.ToolSpec {
	description := "Execute a shell command."
	properties := map[string]interface{}{
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
	}
	if t.fileChangeRecorder() != nil {
		description += " affected_paths bounds best-effort inspection only; use output_claims to declare task deliverables."
		properties["affected_paths"] = map[string]interface{}{
			"type":        "array",
			"items":       map[string]interface{}{"type": "string", "minLength": 1},
			"description": "Optional files or glob patterns (relative to working_dir, or absolute) used only to bound pre/post inspection. This is not an enforced permission boundary and never attributes detected effects.",
		}
		properties["output_claims"] = map[string]interface{}{
			"type":        "array",
			"description": "Pre-execution declarations of intended output. transform is for edits/deletes of existing content; generate is for deliberate deliverables; materialize is for clone/install/download/extract/copy and never contributes authored line totals.",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "minLength": 1},
					"kind": map[string]interface{}{"type": "string", "enum": []string{"transform", "generate", "materialize"}},
				},
				"required": []string{"path", "kind"}, "additionalProperties": false,
			},
		}
	}
	return llm.ToolSpec{
		Name:        ShellToolName,
		Description: description,
		Schema: map[string]interface{}{
			"type":                 "object",
			"properties":           properties,
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
	warning := WarnUnknownParams(args, []string{"command", "working_dir", "timeout_seconds", "description", "env", "affected_paths", "output_claims"})
	errorOutput := func(err error) (llm.ToolOutput, error) {
		if toolErr, ok := err.(*ToolError); ok {
			out := llm.TextOutput(warning + formatToolError(toolErr))
			out.IsError = true
			return out, nil
		}
		message := err.Error()
		if kind := CollaborativeShellErrorKind(err); kind != "" {
			message = fmt.Sprintf("shared shell %s: %s", kind, message)
		}
		out := llm.TextOutput(warning + formatToolError(NewToolError(ErrExecutionFailed, message)))
		out.IsError = true
		return out, nil
	}

	var a ShellArgs
	if err := json.Unmarshal(args, &a); err != nil {
		out := llm.TextOutput(warning + formatToolError(NewToolError(ErrInvalidParams, err.Error())))
		out.IsError = true
		return out, nil
	}
	if a.Command == "" {
		out := llm.TextOutput(warning + formatToolError(NewToolError(ErrInvalidParams, "command is required")))
		out.IsError = true
		return out, nil
	}

	controller, routing := t.collaborativeShell()
	binding, hasBinding := CollaborativeShellRunBindingFromContext(ctx)
	if routing == ShellRoutingControllerRequired && controller == nil {
		return errorOutput(NewCollaborativeShellError("controller_unavailable", "collaborative shell controller is not installed"))
	}
	if routing == ShellRoutingControllerRequired && !hasBinding {
		return errorOutput(NewCollaborativeShellError("controller_unavailable", "collaborative shell run binding is missing"))
	}
	if hasBinding && binding.Required {
		if controller == nil {
			return errorOutput(NewCollaborativeShellError("controller_unavailable", "collaborative shell controller is not installed"))
		}
		if a.WorkingDir != "" || len(a.Env) != 0 || len(a.AffectedPaths) != 0 || len(a.OutputClaims) != 0 {
			out := llm.TextOutput(warning + formatToolError(NewToolError(ErrInvalidParams, "Shared shell commands cannot use working_dir, env, affected_paths, or output_claims. Express directory and environment changes in the command with cd, export, or command prefixes.")))
			out.IsError = true
			return out, nil
		}
		timeout := a.TimeoutSeconds
		if timeout <= 0 {
			timeout = 30
		}
		if timeout > 300 {
			timeout = 300
		}
		if t.approval != nil {
			outcome, err := t.approval.CheckSharedShellApprovalWithContext(ctx, a.Command, approvalTranscriptFromContext(ctx))
			if err != nil {
				return errorOutput(err)
			}
			if outcome == Cancel {
				out := llm.TextOutput(warning + formatToolError(NewToolErrorf(ErrPermissionDenied, "shared shell command not allowed: %s", truncateCommand(a.Command))))
				out.IsError = true
				return out, nil
			}
		}
		result, err := controller.Execute(ctx, llm.SessionIDFromContext(ctx), SharedShellArgs{
			Command: a.Command, TimeoutSeconds: timeout, ToolCallID: llm.CallIDFromContext(ctx),
			OutputLimit: t.limits.MaxBytes, ExpectedShellID: binding.ShellID, ActivityFence: binding.Fence,
		})
		if err != nil {
			if CollaborativeShellErrorKind(err) == "terminal_changed" {
				out := llm.TextOutput(warning + formatShellResult(result, t.limits) + "\n\nshared shell terminal_changed: " + err.Error())
				out.IsError = true
				return out, nil
			}
			if result.RecoveryFailed && CollaborativeShellErrorKind(err) == "recovery_failed" {
				out := llm.TextOutput(warning + formatShellResult(result, t.limits) + "\n\nshared shell recovery_failed: " + err.Error())
				out.TimedOut = result.TimedOut
				out.IsError = true
				return out, nil
			}
			return errorOutput(err)
		}
		out := llm.TextOutput(warning + formatShellResult(result, t.limits))
		out.TimedOut = result.TimedOut
		out.IsError = result.ExitCode != 0 || result.TimedOut || result.Canceled || result.RecoveryFailed
		return out, nil
	}
	return t.executeLocal(ctx, args)
}

func (t *ShellTool) executeLocal(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	recorder := t.fileChangeRecorder()
	trackingEnabled := recorder != nil
	warning := WarnUnknownParams(args, []string{"command", "working_dir", "timeout_seconds", "description", "env", "affected_paths", "output_claims"})
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
	// BaseDir, then process cwd for legacy callers. Unbound daemon runtimes reject
	// the final ambient fallback.
	workDir := ""
	if a.WorkingDir != "" {
		if t.config != nil {
			if t.config.RequiresExplicitWorkingDir() && t.config.WorkingDir() == "" && !filepath.IsAbs(strings.TrimSpace(a.WorkingDir)) {
				return errorOutput(formatToolError(NewToolError(ErrInvalidParams, "relative working_dir requires an absolute path or explicit session working directory"))), nil
			}
			workDir = t.config.ResolveDir(a.WorkingDir)
		} else {
			workDir = resolvePathAgainstBase(a.WorkingDir, "")
		}
	} else if t.config != nil {
		workDir = t.config.ShellDir()
		if workDir == "" && t.config.RequiresExplicitWorkingDir() {
			return errorOutput(formatToolError(NewToolError(ErrInvalidParams, "working_dir is required when the session has no explicit working directory"))), nil
		}
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

	var claims []normalizedOutputClaim
	if trackingEnabled {
		var claimErr error
		claims, claimErr = normalizeOutputClaims(workDir, a.OutputClaims)
		if claimErr == nil {
			claimErr = validateOutputClaimAuthority(t.approval, workDir, claims)
		}
		if claimErr != nil {
			return errorOutput(formatToolError(NewToolError(ErrInvalidParams, claimErr.Error()))), nil
		}
	}

	// Check permissions — pass both command and working directory so the
	// approval UI can show the user where the command will run.
	if t.approval != nil {
		outcome, err := t.approval.checkShellApprovalWithContext(ctx, a.Command, workDir, func() []TranscriptEntry {
			return approvalTranscriptFromContext(ctx)
		})
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
	cleanup = sync.OnceFunc(cleanup)
	defer cleanup()

	stdout := newLimitedBuffer(t.limits.MaxBytes)
	stderr := newLimitedBuffer(t.limits.MaxBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Snapshot relevant files so changes made by the command can be recorded.
	var snap *shellSnapshot
	if trackingEnabled {
		snap = preShellSnapshotWithClaims(ctx, recorder, workDir, a.AffectedPaths, claims)
	}

	// Run command
	err := cmd.Run()
	// Stop descendants before reading the filesystem so tracking captures the
	// state the shell invocation actually leaves behind.
	cleanup()

	// Diff against the snapshot even on timeout or failure — partial writes
	// are real changes.
	tracking := postShellTracking(ctx, recorder, snap)
	trackingText := formatShellTrackingResult(tracking)

	result := ShellResult{
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		ExitCode:        0,
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}

	// Distinguish an elapsed deadline from explicit caller cancellation.
	if execCtx.Err() != nil {
		timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded)
		result.TimedOut = timedOut
		result.Canceled = !timedOut
		return llm.ToolOutput{Content: warning + formatShellResult(result, t.limits) + trackingText, TimedOut: timedOut, IsError: true, FileChanges: tracking.FileChanges, FilesystemObservations: tracking.Observations, OutputClaimDiagnostics: tracking.Diagnostics}, nil
	}

	// Get exit code
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			output := textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "command error: %v", err)) + trackingText)
			output.IsError = true
			output.FileChanges = tracking.FileChanges
			output.FilesystemObservations = tracking.Observations
			output.OutputClaimDiagnostics = tracking.Diagnostics
			return output, nil
		}
	}

	output := textOutput(formatShellResult(result, t.limits) + trackingText)
	output.IsError = result.ExitCode != 0
	output.FileChanges = tracking.FileChanges
	output.FilesystemObservations = tracking.Observations
	output.OutputClaimDiagnostics = tracking.Diagnostics
	return output, nil
}

func formatShellTrackingResult(tracking shellTrackingResult) string {
	if len(tracking.Observations) == 0 && len(tracking.Diagnostics) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nfilesystem_tracking:")
	if len(tracking.Observations) > 0 {
		created, modified, deleted := 0, 0, 0
		for _, observation := range tracking.Observations {
			created += observation.CreatedCount
			modified += observation.ModifiedCount
			deleted += observation.DeletedCount
		}
		fmt.Fprintf(&sb, "\n- %d observation batch(es), not included in agent change totals (%d created, %d modified, %d deleted)", len(tracking.Observations), created, modified, deleted)
	}
	for _, diagnostic := range tracking.Diagnostics {
		fmt.Fprintf(&sb, "\n- %s", diagnostic.Reason)
		if diagnostic.NormalizedPattern != "" {
			fmt.Fprintf(&sb, ": %s", diagnostic.NormalizedPattern)
		}
		if diagnostic.Message != "" {
			fmt.Fprintf(&sb, " (%s)", diagnostic.Message)
		}
	}
	return sb.String()
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

	if result.RecoveryFailed {
		sb.WriteString("[Shared shell recovery failed; collaboration requires attention]\n\n")
	}
	if result.TimedOut {
		sb.WriteString("[Command timed out]\n\n")
	} else if result.Canceled {
		sb.WriteString("[Command canceled]\n\n")
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
