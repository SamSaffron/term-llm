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
	FilePath  string `json:"file_path"`
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
				"file_path": map[string]interface{}{
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
			"required":             []string{"file_path"},
			"additionalProperties": false,
		},
	}
}

func (t *ReadFileTool) Preview(args json.RawMessage) string {
	var a ReadFileArgs
	if err := json.Unmarshal(args, &a); err != nil || a.FilePath == "" {
		return ""
	}
	if a.StartLine > 0 && a.EndLine > 0 {
		return fmt.Sprintf("%s:%d-%d", a.FilePath, a.StartLine, a.EndLine)
	} else if a.StartLine > 0 {
		return fmt.Sprintf("%s:%d-", a.FilePath, a.StartLine)
	} else if a.EndLine > 0 {
		return fmt.Sprintf("%s:1-%d", a.FilePath, a.EndLine)
	}
	return a.FilePath
}

func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a ReadFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return formatToolError(NewToolError(ErrInvalidParams, err.Error())), nil
	}

	if a.FilePath == "" {
		return formatToolError(NewToolError(ErrInvalidParams, "file_path is required")), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(ReadFileToolName, a.FilePath, a.FilePath, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return formatToolError(toolErr), nil
			}
			return formatToolError(NewToolError(ErrPermissionDenied, err.Error())), nil
		}
		if outcome == Cancel {
			return formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.FilePath)), nil
		}
	}

	// Read file
	data, err := os.ReadFile(a.FilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return formatToolError(NewToolError(ErrFileNotFound, a.FilePath)), nil
		}
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "read error: %v", err)), nil
	}

	// Check for binary file
	if isBinaryContent(data) {
		return formatToolError(NewToolErrorf(ErrBinaryFile, "%s appears to be a binary file", a.FilePath)), nil
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
		return formatToolError(NewToolErrorf(ErrInvalidParams, "start_line %d exceeds file length %d", a.StartLine, totalLines)), nil
	}

	end := totalLines
	if a.EndLine > 0 && a.EndLine < totalLines {
		end = a.EndLine
	}

	if start >= end {
		return "", nil
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

	return output, nil
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
