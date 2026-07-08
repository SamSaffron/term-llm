package tools

import (
	"context"
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
	recorder FileChangeRecorder
	config   *ToolConfig
}

// NewWriteFileTool creates a new WriteFileTool.
func NewWriteFileTool(approval *ApprovalManager, configs ...*ToolConfig) *WriteFileTool {
	return &WriteFileTool{
		approval: approval,
		config:   optionalToolConfig(configs),
	}
}

// WriteFileArgs are the arguments for write_file.
type WriteFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFileTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        WriteFileToolName,
		Description: "Create or overwrite a file with the specified content. Creates parent directories if needed.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Path to the file to write",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "Full file content to write",
				},
			},
			"required":             []string{"path", "content"},
			"additionalProperties": false,
		},
	}
}

func (t *WriteFileTool) Preview(args json.RawMessage) string {
	var a WriteFileArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Path == "" {
		return ""
	}
	return a.Path
}

func (t *WriteFileTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	warning := WarnUnknownParams(args, []string{"path", "content"})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}

	var a WriteFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Path == "" {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "path is required"))), nil
	}

	absPath, err := resolveToolPathWithConfig(a.Path, true, t.config)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return textOutput(formatToolError(toolErr)), nil
		}
		return textOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(WriteFileToolName, absPath, a.Path, true)
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

	// Serialize concurrent writes/edits to the same file path so the
	// existing-content read below pairs atomically with the final rename.
	defer lockFilePath(absPath)()

	// Check if file exists for diff info and preserve permissions
	existingContent := ""
	isNew := true
	var existingMode os.FileMode
	if info, err := os.Stat(absPath); err == nil {
		existingMode = info.Mode()
		if data, err := os.ReadFile(absPath); err == nil {
			existingContent = string(data)
			isNew = false
		}
	}

	// Follow symlinks (including dangling ones) so the atomic rename below
	// writes through the link — e.g. renamed handover plan files — instead
	// of replacing the link with a regular file.
	writePath := resolveWriteTarget(absPath)

	// Create parent directories
	dir := filepath.Dir(writePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to create directory: %v", err))), nil
	}

	// Atomic write: write to a uniquely-named temp file, then rename.
	// Using os.CreateTemp avoids a name collision when concurrent calls target the same destination.
	base := filepath.Base(writePath)
	tf, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to create temp file: %v", err))), nil
	}
	tempPath := tf.Name()

	if _, err := tf.Write([]byte(a.Content)); err != nil {
		tf.Close()
		os.Remove(tempPath)
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to write temp file: %v", err))), nil
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		os.Remove(tempPath)
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to sync temp file: %v", err))), nil
	}
	if err := tf.Close(); err != nil {
		os.Remove(tempPath)
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to close temp file: %v", err))), nil
	}

	// Preserve existing file permissions, or use 0644 for new files.
	// CreateTemp creates files with 0600 which is too restrictive for source files.
	mode := existingMode
	if isNew {
		mode = 0644
	}
	if err := os.Chmod(tempPath, mode); err != nil {
		os.Remove(tempPath)
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to set file permissions: %v", err))), nil
	}

	if err := os.Rename(tempPath, writePath); err != nil {
		os.Remove(tempPath)
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to rename temp file: %v", err))), nil
	}

	// Build result message
	output := llm.ToolOutput{}
	if fc := recordFileChange(ctx, t.recorder, WriteFileToolName, absPath, []byte(existingContent), []byte(a.Content), isNew, false); fc != nil {
		output.FileChanges = []llm.FileChange{*fc}
	}
	if isNew {
		output.Content = fmt.Sprintf("Created new file: %s (%d lines).", absPath, countLines(a.Content))

		// Populate structured diff data (skip if content is too large)
		if len(a.Content) < diff.MaxDiffSize {
			output.Diffs = []llm.DiffData{
				{File: absPath, Old: "", New: a.Content, Line: 1, Operation: llm.DiffOperationCreate},
			}
		}
	} else {
		oldLines := countLines(existingContent)
		newLines := countLines(a.Content)
		output.Content = fmt.Sprintf("Updated %s: %d lines -> %d lines.", absPath, oldLines, newLines)

		// Populate structured diff data (skip if content is too large)
		if len(existingContent) < diff.MaxDiffSize && len(a.Content) < diff.MaxDiffSize {
			output.Diffs = []llm.DiffData{
				{File: absPath, Old: existingContent, New: a.Content, Line: 1},
			}
		}
	}

	output.Content = warning + output.Content

	return output, nil
}

// resolveWriteTarget follows symlinks at absPath so atomic temp+rename
// writes land in the link's target instead of replacing the link with a
// regular file. Only links whose target is a plain sibling name in the same
// directory are followed — the handover rename case. Absolute targets or
// targets in other directories are left alone: permission approval happens
// before this resolution, so following them could redirect an approved write
// outside the approved directory (e.g. a dangling link planted in the
// auto-approved handover dir). Dangling same-directory links resolve too —
// the rename then creates the target.
func resolveWriteTarget(absPath string) string {
	for i := 0; i < 8; i++ {
		fi, err := os.Lstat(absPath)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return absPath
		}
		target, err := os.Readlink(absPath)
		if err != nil {
			return absPath
		}
		if filepath.IsAbs(target) || filepath.Dir(target) != "." {
			return absPath
		}
		absPath = filepath.Join(filepath.Dir(absPath), target)
	}
	return absPath
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
