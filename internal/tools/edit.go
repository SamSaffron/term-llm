package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/samsaffron/term-llm/cmd/udiff"
	"github.com/samsaffron/term-llm/internal/diff"
	"github.com/samsaffron/term-llm/internal/edit"
	"github.com/samsaffron/term-llm/internal/llm"
)

// EditFileTool implements the edit_file tool with dual modes.
type EditFileTool struct {
	approval *ApprovalManager
}

// NewEditFileTool creates a new EditFileTool.
func NewEditFileTool(approval *ApprovalManager) *EditFileTool {
	return &EditFileTool{
		approval: approval,
	}
}

// EditFileArgs supports two modes:
// - Mode 1 (Delegated): instructions + optional line_range
// - Mode 2 (Direct): old_text + new_text
type EditFileArgs struct {
	FilePath string `json:"file_path"`
	// Mode 1: Delegated edit (natural language)
	Instructions string `json:"instructions,omitempty"`
	LineRange    string `json:"line_range,omitempty"` // e.g., "10-20"
	// Mode 2: Direct edit (deterministic)
	OldText string `json:"old_text,omitempty"`
	NewText string `json:"new_text,omitempty"`
}

func (t *EditFileTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: EditFileToolName,
		Description: `Edit a file. Two modes available:
1. Direct edit: provide old_text and new_text for deterministic string replacement with 5-level matching
2. The literal token <<<elided>>> in old_text matches any sequence of characters (including newlines)

Use direct edit (old_text/new_text) for simple changes. Avoid mixing modes.`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file_path": map[string]interface{}{
					"type":        "string",
					"description": "Path to the file to edit",
				},
				"old_text": map[string]interface{}{
					"type":        "string",
					"description": "Exact text to find and replace. Include enough context to be unique. You may use <<<elided>>> to match any sequence.",
				},
				"new_text": map[string]interface{}{
					"type":        "string",
					"description": "Text to replace old_text with",
				},
			},
			"required":             []string{"file_path", "old_text", "new_text"},
			"additionalProperties": false,
		},
	}
}

func (t *EditFileTool) Preview(args json.RawMessage) string {
	var a EditFileArgs
	if err := json.Unmarshal(args, &a); err != nil || a.FilePath == "" {
		return ""
	}
	return a.FilePath
}

func (t *EditFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a EditFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return formatToolError(NewToolError(ErrInvalidParams, err.Error())), nil
	}

	if a.FilePath == "" {
		return formatToolError(NewToolError(ErrInvalidParams, "file_path is required")), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(EditFileToolName, a.FilePath, a.FilePath, true)
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

	// Determine mode
	hasInstructions := a.Instructions != ""
	hasDirectEdit := a.OldText != "" || a.NewText != ""

	if hasInstructions && hasDirectEdit {
		return formatToolError(NewToolError(ErrInvalidParams, "cannot mix instructions with old_text/new_text")), nil
	}

	if !hasInstructions && !hasDirectEdit {
		return formatToolError(NewToolError(ErrInvalidParams, "provide either instructions or old_text/new_text")), nil
	}

	if hasDirectEdit {
		return t.executeDirectEdit(ctx, a)
	}

	// Delegated edit not implemented in this tool - it would require an LLM provider
	return formatToolError(NewToolError(ErrInvalidParams, "instructions mode requires the full edit command")), nil
}

// executeDirectEdit performs a deterministic string replacement using 5-level matching.
func (t *EditFileTool) executeDirectEdit(ctx context.Context, a EditFileArgs) (string, error) {
	// Use a lock file to serialize concurrent edits to the same file.
	// We can't lock the file itself because rename() replaces the inode,
	// and other goroutines holding fds to the old inode won't see changes.
	lockPath := a.FilePath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to create lock file: %v", err)), nil
	}
	defer func() {
		lockFile.Close()
		os.Remove(lockPath) // Best-effort cleanup
	}()

	// Acquire exclusive lock (blocks until available)
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to lock: %v", err)), nil
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	// Read file content while holding lock
	data, err := os.ReadFile(a.FilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return formatToolError(NewToolError(ErrFileNotFound, a.FilePath)), nil
		}
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "read error: %v", err)), nil
	}

	content := string(data)
	search := a.OldText

	// Handle <<<elided>>> markers - convert to ... for the match package
	if strings.Contains(search, "<<<elided>>>") {
		search = strings.ReplaceAll(search, "<<<elided>>>", "...")
	}

	// Find match using 5-level matching
	result, err := edit.FindMatch(content, search)
	if err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "could not find old_text: %v", err)), nil
	}

	// Apply the replacement
	newContent := edit.ApplyMatch(content, result, a.NewText)

	// Write back atomically using a unique temp file
	dir := filepath.Dir(a.FilePath)
	base := filepath.Base(a.FilePath)
	tempFile, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to create temp file: %v", err)), nil
	}
	tempPath := tempFile.Name()

	if _, err := tempFile.WriteString(newContent); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to write temp file: %v", err)), nil
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(tempPath)
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to close temp file: %v", err)), nil
	}

	if err := os.Rename(tempPath, a.FilePath); err != nil {
		os.Remove(tempPath)
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to rename temp file: %v", err)), nil
	}

	// Build result message
	oldLines := countLines(result.Original)
	newLines := countLines(a.NewText)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Edited %s: replaced %d lines with %d lines", a.FilePath, oldLines, newLines))
	if result.Level != edit.MatchExact {
		sb.WriteString(" (fuzzy match — old_text did not exactly match file content)")
	}
	sb.WriteString(".")

	// Emit diff marker for streaming display (skip if content is too large)
	if len(result.Original) < diff.MaxDiffSize && len(a.NewText) < diff.MaxDiffSize {
		// Compute starting line number (1-indexed) from byte offset
		startLine := strings.Count(content[:result.Start], "\n") + 1
		diffData := struct {
			File string `json:"f"`
			Old  string `json:"o"`
			New  string `json:"n"`
			Line int    `json:"l"`
		}{a.FilePath, result.Original, a.NewText, startLine}
		if encoded, err := json.Marshal(diffData); err == nil {
			sb.WriteString("\n__DIFF__:" + base64.StdEncoding.EncodeToString(encoded))
		}
	}

	return sb.String(), nil
}

// UnifiedDiffTool implements the unified_diff tool.
type UnifiedDiffTool struct {
	approval *ApprovalManager
}

// NewUnifiedDiffTool creates a new UnifiedDiffTool.
func NewUnifiedDiffTool(approval *ApprovalManager) *UnifiedDiffTool {
	return &UnifiedDiffTool{
		approval: approval,
	}
}

// UnifiedDiffArgs are the arguments for unified_diff.
type UnifiedDiffArgs struct {
	Diff string `json:"diff"`
}

func (t *UnifiedDiffTool) Spec() llm.ToolSpec {
	return llm.UnifiedDiffToolSpec()
}

func (t *UnifiedDiffTool) Preview(args json.RawMessage) string {
	var a UnifiedDiffArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Diff == "" {
		return ""
	}
	// Extract first filename from diff for preview
	lines := strings.Split(a.Diff, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "--- ") {
			path := strings.TrimPrefix(line, "--- ")
			path = strings.TrimPrefix(path, "a/")
			return path
		}
	}
	return "multiple files"
}

func (t *UnifiedDiffTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a UnifiedDiffArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return formatToolError(NewToolError(ErrInvalidParams, err.Error())), nil
	}

	if a.Diff == "" {
		return formatToolError(NewToolError(ErrInvalidParams, "diff is required")), nil
	}

	// Parse the unified diff
	fileDiffs, err := udiff.Parse(a.Diff)
	if err != nil {
		return formatToolError(NewToolErrorf(ErrInvalidParams, "failed to parse diff: %v", err)), nil
	}

	if len(fileDiffs) == 0 {
		return "No changes to apply", nil
	}

	// Check permissions for all files first
	for _, fd := range fileDiffs {
		if t.approval != nil {
			outcome, err := t.approval.CheckPathApproval(UnifiedDiffToolName, fd.Path, fd.Path, true)
			if err != nil {
				if toolErr, ok := err.(*ToolError); ok {
					return formatToolError(toolErr), nil
				}
				return formatToolError(NewToolError(ErrPermissionDenied, err.Error())), nil
			}
			if outcome == Cancel {
				return formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", fd.Path)), nil
			}
		}
	}

	var sb strings.Builder
	var allWarnings []string

	for _, fd := range fileDiffs {
		// Read file content
		data, err := os.ReadFile(fd.Path)
		if err != nil {
			allWarnings = append(allWarnings, fmt.Sprintf("%s: %v", fd.Path, err))
			continue
		}
		content := string(data)

		// Apply the diff
		result := udiff.ApplyWithWarnings(content, fd.Hunks)
		if len(result.Warnings) > 0 {
			allWarnings = append(allWarnings, result.Warnings...)
		}

		// Write back if any changes
		if result.Content != content {
			dir := filepath.Dir(fd.Path)
			base := filepath.Base(fd.Path)
			tempFile, err := os.CreateTemp(dir, "."+base+".*.tmp")
			if err != nil {
				allWarnings = append(allWarnings, fmt.Sprintf("%s: failed to create temp file: %v", fd.Path, err))
				continue
			}
			tempPath := tempFile.Name()

			if _, err := tempFile.WriteString(result.Content); err != nil {
				tempFile.Close()
				os.Remove(tempPath)
				allWarnings = append(allWarnings, fmt.Sprintf("%s: failed to write temp file: %v", fd.Path, err))
				continue
			}
			tempFile.Close()

			if err := os.Rename(tempPath, fd.Path); err != nil {
				os.Remove(tempPath)
				allWarnings = append(allWarnings, fmt.Sprintf("%s: failed to rename: %v", fd.Path, err))
				continue
			}

			oldLines := countLines(content)
			newLines := countLines(result.Content)
			sb.WriteString(fmt.Sprintf("Applied changes to %s: %d lines -> %d lines.\n", fd.Path, oldLines, newLines))

			// Emit diff marker
			if len(content) < diff.MaxDiffSize && len(result.Content) < diff.MaxDiffSize {
				diffData := struct {
					File string `json:"f"`
					Old  string `json:"o"`
					New  string `json:"n"`
					Line int    `json:"l"`
				}{fd.Path, content, result.Content, 1}
				if encoded, err := json.Marshal(diffData); err == nil {
					sb.WriteString("\n__DIFF__:" + base64.StdEncoding.EncodeToString(encoded) + "\n")
				}
			}
		} else {
			sb.WriteString(fmt.Sprintf("No changes for %s.\n", fd.Path))
		}
	}

	if len(allWarnings) > 0 {
		sb.WriteString("\nWarnings:\n")
		for _, w := range allWarnings {
			sb.WriteString("- " + w + "\n")
		}
	}

	return sb.String(), nil
}

// GenerateDiff creates a unified diff between old and new content.
func GenerateDiff(oldContent, newContent, filePath string) string {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- a/%s\n", filePath))
	sb.WriteString(fmt.Sprintf("+++ b/%s\n", filePath))

	// Simple diff - show removed and added lines
	// For a proper unified diff, we'd use a diff algorithm
	maxLines := len(oldLines)
	if len(newLines) > maxLines {
		maxLines = len(newLines)
	}

	for i := 0; i < maxLines; i++ {
		oldLine := ""
		newLine := ""
		if i < len(oldLines) {
			oldLine = oldLines[i]
		}
		if i < len(newLines) {
			newLine = newLines[i]
		}

		if oldLine != newLine {
			if oldLine != "" {
				sb.WriteString(fmt.Sprintf("-%s\n", oldLine))
			}
			if newLine != "" {
				sb.WriteString(fmt.Sprintf("+%s\n", newLine))
			}
		}
	}

	return sb.String()
}
