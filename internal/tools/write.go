package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/diff"
	"github.com/samsaffron/term-llm/internal/llm"
)

// WriteFileTool implements the write_file tool.
type WriteFileTool struct {
	approval *ApprovalManager
}

// NewWriteFileTool creates a new WriteFileTool.
func NewWriteFileTool(approval *ApprovalManager) *WriteFileTool {
	return &WriteFileTool{
		approval: approval,
	}
}

// WriteFileArgs are the arguments for write_file.
type WriteFileArgs struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func (t *WriteFileTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        WriteFileToolName,
		Description: "Create or overwrite a file with the specified content. Creates parent directories if needed.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file_path": map[string]interface{}{
					"type":        "string",
					"description": "Path to the file to write",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "Full file content to write",
				},
			},
			"required":             []string{"file_path", "content"},
			"additionalProperties": false,
		},
	}
}

func (t *WriteFileTool) Preview(args json.RawMessage) string {
	var a WriteFileArgs
	if err := json.Unmarshal(args, &a); err != nil || a.FilePath == "" {
		return ""
	}
	return a.FilePath
}

func (t *WriteFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a WriteFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return formatToolError(NewToolError(ErrInvalidParams, err.Error())), nil
	}

	if a.FilePath == "" {
		return formatToolError(NewToolError(ErrInvalidParams, "file_path is required")), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(WriteFileToolName, a.FilePath, a.FilePath, true)
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

	// Resolve absolute path
	absPath, err := filepath.Abs(a.FilePath)
	if err != nil {
		return formatToolError(NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err)), nil
	}

	// Check if file exists for diff info
	existingContent := ""
	isNew := true
	if data, err := os.ReadFile(absPath); err == nil {
		existingContent = string(data)
		isNew = false
	}

	// Create parent directories
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to create directory: %v", err)), nil
	}

	// Atomic write: write to temp file, then rename
	tempFile := absPath + ".tmp"
	if err := os.WriteFile(tempFile, []byte(a.Content), 0644); err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to write temp file: %v", err)), nil
	}

	if err := os.Rename(tempFile, absPath); err != nil {
		// Clean up temp file on failure
		os.Remove(tempFile)
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to rename temp file: %v", err)), nil
	}

	// Build result message
	var sb strings.Builder
	if isNew {
		sb.WriteString(fmt.Sprintf("Created new file: %s (%d lines).", absPath, countLines(a.Content)))
	} else {
		oldLines := countLines(existingContent)
		newLines := countLines(a.Content)
		sb.WriteString(fmt.Sprintf("Updated %s: %d lines -> %d lines.", absPath, oldLines, newLines))

		// Emit diff marker for streaming display (skip if content is too large)
		if len(existingContent) < diff.MaxDiffSize && len(a.Content) < diff.MaxDiffSize {
			diffData := struct {
				File string `json:"f"`
				Old  string `json:"o"`
				New  string `json:"n"`
				Line int    `json:"l"`
			}{a.FilePath, existingContent, a.Content, 1}
			if encoded, err := json.Marshal(diffData); err == nil {
				sb.WriteString("\n__DIFF__:" + base64.StdEncoding.EncodeToString(encoded))
			}
		}
	}

	return sb.String(), nil
}

// countLines counts the number of lines in a string.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	count := strings.Count(s, "\n")
	// Add 1 if doesn't end with newline
	if !strings.HasSuffix(s, "\n") {
		count++
	}
	return count
}
