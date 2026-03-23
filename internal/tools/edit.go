package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/cmd/udiff"
	"github.com/samsaffron/term-llm/internal/diff"
	"github.com/samsaffron/term-llm/internal/edit"
	"github.com/samsaffron/term-llm/internal/llm"
)

// fileMu serialises concurrent edits to the same file path.
// Both EditFileTool and UnifiedDiffTool share this map so that
// an edit_file and a unified_diff targeting the same path cannot
// race each other.
//
// Entries are never evicted: one *sync.Mutex (~100 bytes) per unique
// resolved path edited during the process lifetime. This is fine for a
// CLI tool that edits tens to hundreds of files per session.
var fileMu sync.Map // absPath (string) -> *sync.Mutex

// lockFilePath acquires a per-path mutex and returns the unlock function.
func lockFilePath(absPath string) func() {
	v, _ := fileMu.LoadOrStore(absPath, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

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
	Path string `json:"path"`
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
				"path": map[string]interface{}{
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
			"required":             []string{"path", "old_text", "new_text"},
			"additionalProperties": false,
		},
	}
}

func (t *EditFileTool) Preview(args json.RawMessage) string {
	var a EditFileArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Path == "" {
		return ""
	}
	return a.Path
}

func (t *EditFileTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	warning := WarnUnknownParams(args, []string{"path", "old_text", "new_text", "instructions", "line_range"})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}

	var a EditFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Path == "" {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "path is required"))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(EditFileToolName, a.Path, a.Path, true)
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

	// Determine mode
	hasInstructions := a.Instructions != ""
	hasDirectEdit := a.OldText != "" || a.NewText != ""

	if hasInstructions && hasDirectEdit {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "cannot mix instructions with old_text/new_text"))), nil
	}

	if !hasInstructions && !hasDirectEdit {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "provide either instructions or old_text/new_text"))), nil
	}

	if hasDirectEdit {
		output, err := t.executeDirectEdit(ctx, a)
		if err != nil {
			return output, err
		}
		output.Content = warning + output.Content
		return output, nil
	}

	// Delegated edit not implemented in this tool - it would require an LLM provider
	return textOutput(formatToolError(NewToolError(ErrInvalidParams, "instructions mode requires the full edit command"))), nil
}

// executeDirectEdit performs a deterministic string replacement using 5-level matching.
func (t *EditFileTool) executeDirectEdit(ctx context.Context, a EditFileArgs) (llm.ToolOutput, error) {
	// Resolve the execution path so concurrent edits of the same underlying
	// file share one lock path and the final write does not follow a late
	// symlink change.
	absPath, err := resolveToolPath(a.Path, true)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return llm.TextOutput(formatToolError(toolErr)), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to resolve path: %v", err))), nil
	}

	// Serialize concurrent edits to the same file path.
	defer lockFilePath(absPath)()

	// Read file content and permissions while holding lock
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return llm.TextOutput(formatToolError(NewToolError(ErrFileNotFound, absPath))), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "stat error: %v", err))), nil
	}
	origMode := info.Mode()
	data, err := os.ReadFile(absPath)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "read error: %v", err))), nil
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
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "could not find old_text: %v", err))), nil
	}

	// Apply the replacement
	newContent := edit.ApplyMatch(content, result, a.NewText)

	// Write back atomically using a unique temp file
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)
	tempFile, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to create temp file: %v", err))), nil
	}
	tempPath := tempFile.Name()

	if _, err := tempFile.WriteString(newContent); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to write temp file: %v", err))), nil
	}
	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to sync temp file: %v", err))), nil
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(tempPath)
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to close temp file: %v", err))), nil
	}

	// Preserve original file permissions (CreateTemp uses 0600)
	if err := os.Chmod(tempPath, origMode); err != nil {
		os.Remove(tempPath)
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to set file permissions: %v", err))), nil
	}

	if err := os.Rename(tempPath, absPath); err != nil {
		os.Remove(tempPath)
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to rename temp file: %v", err))), nil
	}

	// Build result message
	oldLines := countLines(result.Original)
	newLines := countLines(a.NewText)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Edited %s: replaced %d lines with %d lines", absPath, oldLines, newLines))
	if result.Level != edit.MatchExact {
		sb.WriteString(" (fuzzy match — old_text did not exactly match file content)")
	}
	sb.WriteString(".")

	output := llm.ToolOutput{Content: sb.String()}

	// Populate structured diff data (skip if content is too large)
	if len(result.Original) < diff.MaxDiffSize && len(a.NewText) < diff.MaxDiffSize {
		startLine := strings.Count(content[:result.Start], "\n") + 1
		output.Diffs = []llm.DiffData{
			{File: absPath, Old: result.Original, New: a.NewText, Line: startLine},
		}
	}

	return output, nil
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

func (t *UnifiedDiffTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a UnifiedDiffArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Diff == "" {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "diff is required"))), nil
	}

	// Parse the unified diff
	fileDiffs, err := udiff.Parse(a.Diff)
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "failed to parse diff: %v", err))), nil
	}

	if len(fileDiffs) == 0 {
		return llm.TextOutput("No changes to apply"), nil
	}

	// Check permissions for all files first
	for _, fd := range fileDiffs {
		if t.approval != nil {
			outcome, err := t.approval.CheckPathApproval(UnifiedDiffToolName, fd.Path, fd.Path, true)
			if err != nil {
				if toolErr, ok := err.(*ToolError); ok {
					return llm.TextOutput(formatToolError(toolErr)), nil
				}
				return llm.TextOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
			}
			if outcome == Cancel {
				return llm.TextOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", fd.Path))), nil
			}
		}
	}

	var sb strings.Builder
	var allWarnings []string
	var diffs []llm.DiffData

	for _, fd := range fileDiffs {
		absPath, err := resolveToolPath(fd.Path, true)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				allWarnings = append(allWarnings, fmt.Sprintf("%s: %s", fd.Path, toolErr.Message))
				continue
			}
			allWarnings = append(allWarnings, fmt.Sprintf("%s: failed to resolve path: %v", fd.Path, err))
			continue
		}

		// Serialise concurrent unified_diff (and edit_file) calls on the same file.
		status, warnings, d := t.applyFileDiff(absPath, fd)
		sb.WriteString(status)
		allWarnings = append(allWarnings, warnings...)
		if d != nil {
			diffs = append(diffs, *d)
		}
	}

	if len(allWarnings) > 0 {
		sb.WriteString("\nWarnings:\n")
		for _, w := range allWarnings {
			sb.WriteString("- " + w + "\n")
		}
	}

	return llm.ToolOutput{Content: sb.String(), Diffs: diffs}, nil
}

// applyFileDiff applies a single file's hunks while holding a per-path lock.
func (t *UnifiedDiffTool) applyFileDiff(absPath string, fd udiff.FileDiff) (status string, warnings []string, diffData *llm.DiffData) {
	defer lockFilePath(absPath)()

	fileInfo, err := os.Stat(absPath)
	if err != nil {
		return "", []string{fmt.Sprintf("%s: %v", fd.Path, err)}, nil
	}
	fileMode := fileInfo.Mode()
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", []string{fmt.Sprintf("%s: %v", fd.Path, err)}, nil
	}
	content := string(data)

	result := udiff.ApplyWithWarnings(content, fd.Hunks)
	if len(result.Warnings) > 0 {
		warnings = append(warnings, result.Warnings...)
	}

	if result.Content == content {
		return fmt.Sprintf("No changes for %s.\n", fd.Path), warnings, nil
	}

	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)
	tempFile, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return "", append(warnings, fmt.Sprintf("%s: failed to create temp file: %v", fd.Path, err)), nil
	}
	tempPath := tempFile.Name()

	if _, err := tempFile.WriteString(result.Content); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return "", append(warnings, fmt.Sprintf("%s: failed to write temp file: %v", fd.Path, err)), nil
	}
	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return "", append(warnings, fmt.Sprintf("%s: failed to sync temp file: %v", fd.Path, err)), nil
	}
	tempFile.Close()

	if err := os.Chmod(tempPath, fileMode); err != nil {
		os.Remove(tempPath)
		return "", append(warnings, fmt.Sprintf("%s: failed to set permissions: %v", fd.Path, err)), nil
	}

	if err := os.Rename(tempPath, absPath); err != nil {
		os.Remove(tempPath)
		return "", append(warnings, fmt.Sprintf("%s: failed to rename: %v", fd.Path, err)), nil
	}

	oldLines := countLines(content)
	newLines := countLines(result.Content)
	status = fmt.Sprintf("Applied changes to %s: %d lines -> %d lines.\n", fd.Path, oldLines, newLines)

	if len(content) < diff.MaxDiffSize && len(result.Content) < diff.MaxDiffSize {
		diffData = &llm.DiffData{
			File: absPath, Old: content, New: result.Content, Line: 1,
		}
	}

	return status, warnings, diffData
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
