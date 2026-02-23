package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ReadFileTool implements the read_file tool.
type ReadFileTool struct {
	approval *ApprovalManager
	limits   OutputLimits
}

// NewReadFileTool creates a new ReadFileTool.
func NewReadFileTool(approval *ApprovalManager, limits OutputLimits) *ReadFileTool {
	return &ReadFileTool{
		approval: approval,
		limits:   limits,
	}
}

// ReadFileArgs are the arguments for read_file.
type ReadFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

func (t *ReadFileTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        ReadFileToolName,
		Description: "Read file contents. Returns line-numbered output. Use start_line/end_line for pagination.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Absolute or relative path to the file to read",
				},
				"start_line": map[string]interface{}{
					"type":        "integer",
					"description": "1-indexed start line (default: 1)",
				},
				"end_line": map[string]interface{}{
					"type":        "integer",
					"description": "1-indexed end line (default: EOF)",
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

func (t *ReadFileTool) Preview(args json.RawMessage) string {
	var a ReadFileArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Path == "" {
		return ""
	}
	if a.StartLine > 0 && a.EndLine > 0 {
		return fmt.Sprintf("%s:%d-%d", a.Path, a.StartLine, a.EndLine)
	} else if a.StartLine > 0 {
		return fmt.Sprintf("%s:%d-", a.Path, a.StartLine)
	} else if a.EndLine > 0 {
		return fmt.Sprintf("%s:1-%d", a.Path, a.EndLine)
	}
	return a.Path
}

func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	warning := WarnUnknownParams(args, []string{"path", "start_line", "end_line"})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}

	var a ReadFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Path == "" {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "path is required"))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(ReadFileToolName, a.Path, a.Path, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return textOutput(formatToolError(toolErr)), nil
			}
			return textOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return textOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.Path))), nil
		}
	}

	// Read file
	data, err := os.ReadFile(a.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return textOutput(formatToolError(NewToolError(ErrFileNotFound, a.Path))), nil
		}
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "read error: %v", err))), nil
	}

	// Check for binary file
	if isBinaryContent(data) {
		return textOutput(formatToolError(NewToolErrorf(ErrBinaryFile, "%s appears to be a binary file", a.Path))), nil
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	// Handle line range
	start := 0
	if a.StartLine > 0 {
		start = a.StartLine - 1
	}
	if start >= totalLines {
		return textOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "start_line %d exceeds file length %d", a.StartLine, totalLines))), nil
	}

	end := totalLines
	if a.EndLine > 0 && a.EndLine < totalLines {
		end = a.EndLine
	}

	if start >= end {
		return textOutput("No content in requested range."), nil
	}

	selectedLines := lines[start:end]

	// Check limits
	truncated := false
	if len(selectedLines) > t.limits.MaxLines {
		selectedLines = selectedLines[:t.limits.MaxLines]
		truncated = true
	}

	// Format output with line numbers
	var sb strings.Builder
	for i, line := range selectedLines {
		lineNum := start + i + 1 // 1-indexed
		sb.WriteString(fmt.Sprintf("%d: %s\n", lineNum, line))
	}

	output := strings.TrimSuffix(sb.String(), "\n")

	// Check byte limit
	if int64(len(output)) > t.limits.MaxBytes {
		output = output[:t.limits.MaxBytes]
		truncated = true
	}

	if truncated {
		output += fmt.Sprintf("\n\n[Output truncated. Total lines: %d. Use start_line/end_line for pagination.]", totalLines)
	}

	return textOutput(output), nil
}

// isBinaryContent detects if content is binary using http.DetectContentType.
func isBinaryContent(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	// Check first 512 bytes
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}

	contentType := http.DetectContentType(sample)

	// Text types are not binary
	if strings.HasPrefix(contentType, "text/") {
		return false
	}

	// application/json, application/xml, etc. are text-like
	if strings.Contains(contentType, "json") || strings.Contains(contentType, "xml") {
		return false
	}

	// Check for null bytes (common in binary)
	for _, b := range sample {
		if b == 0 {
			return true
		}
	}

	return false
}

// formatToolError formats a ToolError for LLM consumption.
func formatToolError(err *ToolError) string {
	return fmt.Sprintf("Error [%s]: %s", err.Type, err.Message)
}
