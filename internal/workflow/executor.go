package workflow

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/samsaffron/term-llm/internal/procutil"
)

const (
	maxAgentOutputBytes    = 8 << 20
	maxEvidenceOutputBytes = 1 << 20
	maxArtifactBytes       = 16 << 20
)

// AgentRequest describes one lazy Lua agent task.
type AgentRequest struct {
	Prompt        string              `json:"prompt"`
	SystemMessage string              `json:"system_message,omitempty"`
	Agent         string              `json:"agent,omitempty"`
	Provider      string              `json:"provider,omitempty"`
	Label         string              `json:"label,omitempty"`
	CWD           string              `json:"cwd"`
	Tools         []string            `json:"tools,omitempty"`
	ReadDirs      []string            `json:"read_dirs,omitempty"`
	WriteDirs     []string            `json:"write_dirs,omitempty"`
	ShellAllow    []string            `json:"shell_allow,omitempty"`
	MaxTurns      int                 `json:"max_turns,omitempty"`
	Dynamic       bool                `json:"dynamic,omitempty"`
	Require       *CommandRequirement `json:"require,omitempty"`
}

// CommandRequirement is deterministic evidence checked after an agent exits.
type CommandRequirement struct {
	Command        string `json:"command"`
	ExitCode       int    `json:"exit_code"`
	Repetitions    int    `json:"repetitions"`
	OutputContains string `json:"output_contains,omitempty"`
	ArtifactGlob   string `json:"artifact_glob,omitempty"`
}

// ToolCall records one actual tool execution reported by the child event stream.
type ToolCall struct {
	CallID  string         `json:"call_id"`
	Name    string         `json:"name"`
	Args    map[string]any `json:"args,omitempty"`
	Success bool           `json:"success"`
}

// Artifact records a file selected by the completion contract.
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// CommandEvidence records one harness-executed completion check.
type CommandEvidence struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// AgentResult captures both output streams from an agent subprocess.
type AgentResult struct {
	Stdout     string            `json:"text"`
	Stderr     string            `json:"stderr,omitempty"`
	ExitReason string            `json:"exit_reason,omitempty"`
	ToolCalls  []ToolCall        `json:"tool_calls,omitempty"`
	Artifacts  []Artifact        `json:"artifacts,omitempty"`
	Evidence   []CommandEvidence `json:"evidence,omitempty"`
}

// AgentExecutor executes one agent request. Tests can inject a fake executor.
type AgentExecutor interface {
	Execute(context.Context, AgentRequest) (AgentResult, error)
}

// CommandExecutor runs agents by spawning the term-llm executable.
type CommandExecutor struct {
	Executable string
}

// Execute invokes: term-llm --no-session ask --porcelain [overrides] <prompt>.
func (e CommandExecutor) Execute(ctx context.Context, request AgentRequest) (AgentResult, error) {
	if strings.TrimSpace(e.Executable) == "" {
		return AgentResult{}, fmt.Errorf("workflow agent executable is required")
	}
	args := []string{"--no-session", "ask", "--no-search", "--skills", "none"}
	if request.Dynamic {
		args = append(args, "--json")
	} else {
		args = append(args, "--porcelain")
	}
	if request.Agent != "" {
		args = append(args, "--agent", request.Agent)
	}
	if request.Provider != "" {
		args = append(args, "--provider", request.Provider)
	}
	if request.SystemMessage != "" {
		args = append(args, "--system-message", request.SystemMessage)
	}
	if request.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprint(request.MaxTurns))
	}
	if len(request.Tools) > 0 {
		args = append(args, "--tools", strings.Join(request.Tools, ","), "--yolo")
	}
	for _, directory := range request.ReadDirs {
		args = append(args, "--read-dir", directory)
	}
	for _, directory := range request.WriteDirs {
		args = append(args, "--write-dir", directory)
	}
	for _, pattern := range request.ShellAllow {
		args = append(args, "--shell-allow", pattern)
	}
	args = append(args, "--", request.Prompt)

	command := exec.CommandContext(ctx, e.Executable, args...)
	procutil.ConfigureDetachedCommand(command)
	command.Dir = request.CWD
	stdout := procutil.NewLimitedBuffer(maxAgentOutputBytes)
	stderr := procutil.NewLimitedBuffer(maxAgentOutputBytes)
	command.Stdout = stdout
	command.Stderr = stderr
	commandErr := command.Run()
	result := AgentResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitReason: "completed"}
	var protocolErr error
	if stdout.Truncated() || stderr.Truncated() {
		protocolErr = fmt.Errorf("agent output exceeded %d bytes", maxAgentOutputBytes)
	} else if request.Dynamic {
		result, protocolErr = parseAgentJSONL(stdout.String(), stderr.String())
	}
	agentErr := commandErr
	if protocolErr != nil {
		if agentErr == nil {
			agentErr = protocolErr
		} else {
			agentErr = fmt.Errorf("%v; parse agent event stream: %w", agentErr, protocolErr)
		}
	}
	if agentErr != nil {
		result.ExitReason = "agent_error"
	}

	if request.Require != nil {
		evidenceErr := verifyCommandRequirement(ctx, request, &result)
		if evidenceErr == nil {
			result.ExitReason = "requirement_satisfied"
			return result, nil
		}
		if agentErr != nil {
			return result, fmt.Errorf("agent command failed: %v; completion requirement: %w", agentErr, evidenceErr)
		}
		return result, evidenceErr
	}
	if agentErr != nil {
		message := fmt.Sprintf("agent command failed: %v", agentErr)
		if trimmed := strings.TrimSpace(result.Stderr); trimmed != "" {
			message += ": " + trimmed
		}
		return result, fmt.Errorf("%s", message)
	}
	return result, nil
}

func parseAgentJSONL(raw, stderr string) (AgentResult, error) {
	result := AgentResult{Stderr: stderr, ExitReason: "completed"}
	calls := make(map[string]int)
	sawDone := false
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return result, fmt.Errorf("invalid agent JSONL at line %d: %w", line, err)
		}
		typeName, _ := event["type"].(string)
		switch typeName {
		case "text.delta":
			if text, ok := event["text"].(string); ok {
				result.Stdout += text
			}
		case "tool.started":
			call := ToolCall{CallID: stringField(event, "call_id"), Name: stringField(event, "name")}
			if args, ok := event["args"].(map[string]any); ok {
				call.Args = args
			}
			calls[call.CallID] = len(result.ToolCalls)
			result.ToolCalls = append(result.ToolCalls, call)
		case "tool.completed":
			if index, ok := calls[stringField(event, "call_id")]; ok {
				result.ToolCalls[index].Success, _ = event["success"].(bool)
			}
		case "done":
			sawDone = true
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("scan agent JSONL: %w", err)
	}
	if !sawDone {
		return result, fmt.Errorf("agent JSONL ended without a done event")
	}
	return result, nil
}

func stringField(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func verifyCommandRequirement(ctx context.Context, request AgentRequest, result *AgentResult) error {
	requirement := request.Require
	if strings.TrimSpace(requirement.Command) == "" {
		return fmt.Errorf("completion requirement command is empty")
	}
	allowed := false
	for _, pattern := range request.ShellAllow {
		if matched, _ := doublestar.Match(pattern, requirement.Command); matched {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("completion command %q is not allowed by the dynamic agent shell policy", requirement.Command)
	}
	repetitions := requirement.Repetitions
	if repetitions <= 0 {
		repetitions = 1
	}
	for range repetitions {
		command := exec.CommandContext(ctx, "sh", "-c", requirement.Command)
		procutil.ConfigureDetachedCommand(command)
		command.Dir = request.CWD
		output := procutil.NewLimitedBuffer(maxEvidenceOutputBytes)
		command.Stdout = output
		command.Stderr = output
		err := command.Run()
		if output.Truncated() {
			return fmt.Errorf("completion command output exceeded %d bytes", maxEvidenceOutputBytes)
		}
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				return fmt.Errorf("run completion command: %w", err)
			}
		}
		text := output.String()
		result.Evidence = append(result.Evidence, CommandEvidence{Command: requirement.Command, ExitCode: exitCode, Output: text})
		if exitCode != requirement.ExitCode {
			return fmt.Errorf("completion command exit code %d, want %d", exitCode, requirement.ExitCode)
		}
		if requirement.OutputContains != "" && !strings.Contains(text, requirement.OutputContains) {
			return fmt.Errorf("completion command output does not contain %q", requirement.OutputContains)
		}
	}
	if requirement.ArtifactGlob != "" {
		matches, err := filepath.Glob(filepath.Join(request.CWD, requirement.ArtifactGlob))
		if err != nil {
			return fmt.Errorf("match completion artifacts: %w", err)
		}
		if len(matches) == 0 {
			return fmt.Errorf("completion artifact glob %q matched no files", requirement.ArtifactGlob)
		}
		for _, match := range matches {
			if !pathWithinAny(match, []string{request.CWD}) {
				return fmt.Errorf("completion artifact %q escapes working directory", match)
			}
			info, err := os.Stat(match)
			if err != nil {
				return fmt.Errorf("stat completion artifact: %w", err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("completion artifact %q is not a regular file", match)
			}
			if info.Size() > maxArtifactBytes {
				return fmt.Errorf("completion artifact %q exceeds %d bytes", match, maxArtifactBytes)
			}
			content, err := os.ReadFile(match)
			if err != nil {
				return fmt.Errorf("read completion artifact: %w", err)
			}
			digest := sha256.Sum256(content)
			result.Artifacts = append(result.Artifacts, Artifact{Path: match, SHA256: hex.EncodeToString(digest[:]), Bytes: info.Size()})
		}
		slices.SortFunc(result.Artifacts, func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
	}
	return nil
}
