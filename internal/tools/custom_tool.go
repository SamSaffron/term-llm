package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm"
)

// validCustomToolNameRE matches valid custom tool names.
var validCustomToolNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// CustomScriptTool implements llm.Tool for a script-backed custom tool
// declared in agent.yaml under tools.custom.
type CustomScriptTool struct {
	def      agents.CustomToolDef
	agentDir string
	limits   OutputLimits
}

// newCustomScriptTool creates a CustomScriptTool from a definition and agent directory.
func newCustomScriptTool(def agents.CustomToolDef, agentDir string, limits OutputLimits) *CustomScriptTool {
	return &CustomScriptTool{
		def:      def,
		agentDir: agentDir,
		limits:   limits,
	}
}

// Spec returns the tool spec for the LLM.
func (t *CustomScriptTool) Spec() llm.ToolSpec {
	schema := t.def.Input
	if schema == nil {
		schema = map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{},
			"additionalProperties": false,
		}
	}
	return llm.ToolSpec{
		Name:        t.def.Name,
		Description: t.def.Description,
		Schema:      schema,
	}
}

// Preview returns a short preview string for display in the UI.
func (t *CustomScriptTool) Preview(args json.RawMessage) string {
	s := string(args)
	if s == "" || s == "{}" || s == "null" {
		return t.def.Name
	}
	preview := t.def.Name + " " + s
	if len(preview) > 50 {
		preview = preview[:47] + "..."
	}
	return preview
}

// Execute runs the custom script with the LLM's args as JSON on stdin.
func (t *CustomScriptTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	// Validate agentDir is set
	if t.agentDir == "" {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "no agent directory configured"))), nil
	}

	// Resolve and validate the script path
	scriptPath, err := t.resolveScript()
	if err != nil {
		return llm.TextOutput(formatToolError(err.(*ToolError))), nil
	}

	// Determine timeout
	timeout := 30
	if t.def.TimeoutSeconds > 0 {
		timeout = t.def.TimeoutSeconds
	}
	if timeout > 300 {
		timeout = 300
	}

	// Working directory: same as the term-llm process cwd
	workDir, err := os.Getwd()
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot get working directory: %v", err))), nil
	}

	// Normalise args: nil → empty object
	if args == nil || string(args) == "null" {
		args = json.RawMessage("{}")
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// Build the command according to the calling convention.
	cmd, err := t.buildCommand(execCtx, scriptPath, args)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "build command: %v", err))), nil
	}
	cmd.Dir = workDir

	// Build environment: inherit + agent-specific vars + per-tool env
	env := os.Environ()
	env = append(env, fmt.Sprintf("TERM_LLM_AGENT_DIR=%s", t.agentDir))
	env = append(env, fmt.Sprintf("TERM_LLM_TOOL_NAME=%s", t.def.Name))
	for k, v := range t.def.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	execErr := cmd.Run()

	result := ShellResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
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

// buildCommand constructs the exec.Cmd for the script according to the calling convention.
//
//	""/"args"   → script --key1 val1 --key2 val2  (named flags, default)
//	"positional" → script val1 val2 …              (values in schema property order)
//	"json"      → script  with JSON piped to stdin
func (t *CustomScriptTool) buildCommand(ctx context.Context, scriptPath string, args json.RawMessage) (*exec.Cmd, error) {
	call := strings.ToLower(strings.TrimSpace(t.def.Call))

	// Parse JSON args into a map
	var argMap map[string]json.RawMessage
	if err := json.Unmarshal(args, &argMap); err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	switch call {
	case "json":
		// Pass raw JSON on stdin
		cmd := exec.CommandContext(ctx, detectShell(), "-c", scriptPath)
		cmd.Stdin = bytes.NewReader(args)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return cmd, nil

	case "positional":
		// Values in schema property order (alphabetical fallback if no order defined)
		keys := propertyOrder(t.def.Input)
		cmdArgs := []string{scriptPath}
		for _, k := range keys {
			if v, ok := argMap[k]; ok {
				cmdArgs = append(cmdArgs, jsonValueToString(v))
			}
		}
		cmd := exec.CommandContext(ctx, detectShell(), append([]string{"-c"}, shellJoin(cmdArgs)...)...)
		cmd.Stdin = strings.NewReader("")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return cmd, nil

	default: // "" or "args" — named flags (--key value)
		keys := sortedKeys(argMap)
		cmdArgs := []string{scriptPath}
		for _, k := range keys {
			cmdArgs = append(cmdArgs, "--"+k, jsonValueToString(argMap[k]))
		}
		cmd := exec.CommandContext(ctx, detectShell(), append([]string{"-c"}, shellJoin(cmdArgs)...)...)
		cmd.Stdin = strings.NewReader("")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return cmd, nil
	}
}

// propertyOrder returns property keys in the order defined by the JSON Schema,
// falling back to sorted order when no explicit ordering is present.
// JSON Schema doesn't guarantee property order, but in practice Go's yaml→map
// loses order. We sort alphabetically as a stable fallback.
func propertyOrder(input map[string]interface{}) []string {
	if input == nil {
		return nil
	}
	props, _ := input["properties"].(map[string]interface{})
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedKeys returns the keys of a map in sorted order.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jsonValueToString converts a JSON value to a plain string suitable for a CLI arg.
// Strings are unquoted; numbers/bools/nulls use their JSON representation.
func jsonValueToString(v json.RawMessage) string {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return s // already a string — strip quotes
	}
	return string(v) // number, bool, null — use as-is
}

// shellJoin builds a shell command string from a slice of args, quoting as needed.
func shellJoin(args []string) []string {
	// We pass the whole thing as a single "-c" argument to the shell,
	// so we need to produce one properly-quoted string.
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuote(a)
	}
	return []string{strings.Join(parts, " ")}
}

// shellQuote wraps a string in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// resolveScript resolves and validates the script path within the agent directory.
// Returns the absolute real path on success, or a *ToolError on failure.
func (t *CustomScriptTool) resolveScript() (string, error) {
	agentDir, err := filepath.Abs(t.agentDir)
	if err != nil {
		return "", NewToolErrorf(ErrExecutionFailed, "resolve agent dir: %v", err)
	}

	// Join script path (relative) onto agent dir
	scriptPath := filepath.Join(agentDir, t.def.Script)
	absScript, err := filepath.Abs(scriptPath)
	if err != nil {
		return "", NewToolErrorf(ErrExecutionFailed, "resolve script path: %v", err)
	}

	// Verify the joined path is still inside the agent dir (pre-symlink check)
	if !strings.HasPrefix(absScript, agentDir+string(filepath.Separator)) {
		return "", NewToolError(ErrSymlinkEscape, "script path escapes agent directory")
	}

	// Resolve symlinks for the final containment check
	realScript, err := filepath.EvalSymlinks(absScript)
	if err != nil {
		if os.IsNotExist(err) {
			return "", NewToolErrorf(ErrFileNotFound, "script not found: %s", t.def.Script)
		}
		return "", NewToolErrorf(ErrExecutionFailed, "resolve symlinks: %v", err)
	}

	realAgentDir, err := filepath.EvalSymlinks(agentDir)
	if err != nil {
		return "", NewToolErrorf(ErrExecutionFailed, "resolve agent dir symlinks: %v", err)
	}

	if !strings.HasPrefix(realScript, realAgentDir+string(filepath.Separator)) {
		return "", NewToolError(ErrSymlinkEscape, "script symlink escapes agent directory")
	}

	// Must be a regular file
	info, err := os.Stat(realScript)
	if err != nil {
		return "", NewToolErrorf(ErrFileNotFound, "script not found: %s", t.def.Script)
	}
	if info.IsDir() {
		return "", NewToolErrorf(ErrInvalidParams, "script target is a directory, not a file: %s", t.def.Script)
	}

	return realScript, nil
}

// RegisterCustomTools registers script-backed custom tools from agent.yaml into the registry.
// It validates that custom tool names don't collide with built-in tool names.
// A startup warning (not an error) is emitted if a script file doesn't exist yet.
func (r *LocalToolRegistry) RegisterCustomTools(defs []agents.CustomToolDef, agentDir string) error {
	for _, def := range defs {
		// Validate name format (belt-and-suspenders; agent.Validate() also checks this)
		if !validCustomToolNameRE.MatchString(def.Name) {
			return fmt.Errorf("custom tool %q: name must match ^[a-z][a-z0-9_]*$", def.Name)
		}

		// Check for collision with built-in tool names
		if ValidToolName(def.Name) {
			return fmt.Errorf("custom tool %q collides with a built-in tool name", def.Name)
		}

		// Warn if the script doesn't exist yet (non-fatal — may be created later)
		if agentDir != "" {
			scriptPath := filepath.Join(agentDir, def.Script)
			if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: custom tool %q script not found: %s\n", def.Name, scriptPath)
			}
		}

		tool := newCustomScriptTool(def, agentDir, r.limits)
		r.tools[def.Name] = tool
	}
	return nil
}
