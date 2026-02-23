package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/samsaffron/term-llm/internal/llm"
)

// GlobTool implements the glob tool.
type GlobTool struct {
	approval *ApprovalManager
}

// NewGlobTool creates a new GlobTool.
func NewGlobTool(approval *ApprovalManager) *GlobTool {
	return &GlobTool{
		approval: approval,
	}
}

// GlobArgs are the arguments for glob.
type GlobArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

// FileEntry represents a file in glob results.
type FileEntry struct {
	FilePath  string    `json:"file_path"`
	IsDir     bool      `json:"is_dir"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
}

const maxGlobResults = 200

func (t *GlobTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        GlobToolName,
		Description: "Find files by glob pattern (supports ** for recursive matching). Returns file metadata sorted by modification time.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"description": "Glob pattern supporting ** for recursive matching, e.g., '**/*.go' or 'src/**/*.ts'",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Base directory for the search (defaults to current directory)",
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
	}
}

func (t *GlobTool) Preview(args json.RawMessage) string {
	var a GlobArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Pattern == "" {
		return ""
	}
	if a.Path != "" {
		return fmt.Sprintf("%s in %s", a.Pattern, a.Path)
	}
	return a.Pattern
}

func (t *GlobTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	warning := WarnUnknownParams(args, []string{"pattern", "path"})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}

	var a GlobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Pattern == "" {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "pattern is required"))), nil
	}

	// Set defaults
	basePath := a.Path
	if basePath == "" {
		var err error
		basePath, err = os.Getwd()
		if err != nil {
			return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot get working directory: %v", err))), nil
		}
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(GlobToolName, basePath, a.Pattern, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return textOutput(formatToolError(toolErr)), nil
			}
			return textOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return textOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", basePath))), nil
		}
	}

	// Resolve base path to absolute
	absBasePath, err := filepath.Abs(basePath)
	if err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot resolve path: %v", err))), nil
	}

	// Find matching files by walking the directory
	var entries []FileEntry
	pattern := a.Pattern

	err = filepath.WalkDir(absBasePath, func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil // Skip errors
		}

		// Skip hidden directories
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}

		// Skip hidden files
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		// Get relative path for matching
		relPath, err := filepath.Rel(absBasePath, path)
		if err != nil {
			return nil
		}

		// Check if pattern matches
		matched, err := doublestar.Match(pattern, relPath)
		if err != nil {
			return nil
		}

		if !matched {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		entries = append(entries, FileEntry{
			FilePath:  path,
			IsDir:     d.IsDir(),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
		})

		if len(entries) >= maxGlobResults {
			return filepath.SkipAll
		}

		return nil
	})

	if err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "walk error: %v", err))), nil
	}

	// Sort by modification time (newest first)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ModTime.After(entries[j].ModTime)
	})

	if len(entries) == 0 {
		return textOutput("No files matched the pattern."), nil
	}

	return textOutput(formatGlobResults(entries, len(entries) >= maxGlobResults)), nil
}

// formatGlobResults formats glob results for the LLM.
func formatGlobResults(entries []FileEntry, truncated bool) string {
	var sb strings.Builder

	for _, e := range entries {
		typeIndicator := "f"
		if e.IsDir {
			typeIndicator = "d"
		}

		// Format size
		size := formatSize(e.SizeBytes)

		// Format time
		timeStr := e.ModTime.Format("2006-01-02 15:04")

		sb.WriteString(fmt.Sprintf("[%s] %s  %s  %s\n", typeIndicator, size, timeStr, e.FilePath))
	}

	if truncated {
		sb.WriteString(fmt.Sprintf("\n[Results truncated at %d files]", maxGlobResults))
	}

	return strings.TrimSuffix(sb.String(), "\n")
}

// formatSize formats a byte count as human-readable.
func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%4dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%4.0f%c", float64(bytes)/float64(div), "KMGTPE"[exp])
}
