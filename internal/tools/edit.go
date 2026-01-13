package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
	FilePath     string `json:"file_path"`
	// Mode 1: Delegated edit (natural language)
	Instructions string `json:"instructions,omitempty"`
	LineRange    string `json:"line_range,omitempty"` // e.g., "10-20"
	// Mode 2: Direct edit (deterministic)
	OldText      string `json:"old_text,omitempty"`
	NewText      string `json:"new_text,omitempty"`
}

func (t *EditFileTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        EditFileToolName,
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
	// Read file
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

	// Write back atomically
	tempFile := a.FilePath + ".tmp"
	if err := os.WriteFile(tempFile, []byte(newContent), 0644); err != nil {
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to write temp file: %v", err)), nil
	}

	if err := os.Rename(tempFile, a.FilePath); err != nil {
		os.Remove(tempFile)
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to rename temp file: %v", err)), nil
	}

	// Build result message
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Edited %s (match level: %s)\n", a.FilePath, result.Level.String()))
	sb.WriteString(fmt.Sprintf("Replaced %d bytes with %d bytes", len(result.Original), len(a.NewText)))

	// Show a brief diff summary
	oldLines := countLines(result.Original)
	newLines := countLines(a.NewText)
	if oldLines != newLines {
		sb.WriteString(fmt.Sprintf("\nLines: %d -> %d", oldLines, newLines))
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
