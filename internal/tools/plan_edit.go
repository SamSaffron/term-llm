package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// Tool names for plan editing.
const (
	AddLineToolName    = "add_line"
	RemoveLineToolName = "remove_line"
)

// AddLineArgs represents the arguments to the add_line tool.
type AddLineArgs struct {
	Content string `json:"content"` // Line content to add (required)
	After   string `json:"after"`   // Text snippet from line to insert after (fuzzy match)
}

// RemoveLineArgs represents the arguments to the remove_line tool.
type RemoveLineArgs struct {
	Match string `json:"match"` // Text snippet to match the line to remove (fuzzy match)
}

// AddLineExecutor is called to execute add_line operations.
// Returns a summary of what was done and the line index where inserted.
type AddLineExecutor func(content string, afterText string) (string, error)

// RemoveLineExecutor is called to execute remove_line operations.
// Returns a summary of what was done.
type RemoveLineExecutor func(matchText string) (string, error)

// AddLineTool implements the add_line tool for the planner agent.
type AddLineTool struct {
	executor AddLineExecutor
}

// NewAddLineTool creates a new add_line tool.
func NewAddLineTool() *AddLineTool {
	return &AddLineTool{}
}

// SetExecutor sets the function that executes add_line operations.
func (t *AddLineTool) SetExecutor(exec AddLineExecutor) {
	t.executor = exec
}

// Spec returns the tool specification.
func (t *AddLineTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        AddLineToolName,
		Description: addLineDescription,
		Schema:      addLineSchema(),
	}
}

// Execute runs the add_line tool.
func (t *AddLineTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.executor == nil {
		return "", NewToolError(ErrExecutionFailed, "add_line executor not configured")
	}

	var parsed AddLineArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "invalid arguments: %v", err)
	}

	if parsed.Content == "" {
		return "", NewToolError(ErrInvalidParams, "content is required")
	}

	return t.executor(parsed.Content, parsed.After)
}

// Preview returns a preview of the add_line operation.
func (t *AddLineTool) Preview(args json.RawMessage) string {
	var parsed AddLineArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return "Add line"
	}
	return "Add: " + truncatePreview(parsed.Content, 50)
}

// RemoveLineTool implements the remove_line tool for the planner agent.
type RemoveLineTool struct {
	executor RemoveLineExecutor
}

// NewRemoveLineTool creates a new remove_line tool.
func NewRemoveLineTool() *RemoveLineTool {
	return &RemoveLineTool{}
}

// SetExecutor sets the function that executes remove_line operations.
func (t *RemoveLineTool) SetExecutor(exec RemoveLineExecutor) {
	t.executor = exec
}

// Spec returns the tool specification.
func (t *RemoveLineTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        RemoveLineToolName,
		Description: removeLineDescription,
		Schema:      removeLineSchema(),
	}
}

// Execute runs the remove_line tool.
func (t *RemoveLineTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.executor == nil {
		return "", NewToolError(ErrExecutionFailed, "remove_line executor not configured")
	}

	var parsed RemoveLineArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "invalid arguments: %v", err)
	}

	if parsed.Match == "" {
		return "", NewToolError(ErrInvalidParams, "match is required")
	}

	return t.executor(parsed.Match)
}

// Preview returns a preview of the remove_line operation.
func (t *RemoveLineTool) Preview(args json.RawMessage) string {
	var parsed RemoveLineArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return "Remove line"
	}
	return "Remove: " + truncatePreview(parsed.Match, 50)
}

func truncatePreview(s string, maxLen int) string {
	// Take first line only
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[:idx]
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

const addLineDescription = `Add one or more lines to the plan document.

Lines are inserted after a line matching the 'after' text. If 'after' is not provided or
no match is found, lines are appended at the end.

The content can contain multiple lines separated by newlines - each line streams to the UI immediately.
Use this to add entire sections at once for efficient editing.`

func addLineSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Line(s) to add. Can contain multiple lines separated by newlines - each streams to UI immediately.",
			},
			"after": map[string]interface{}{
				"type":        "string",
				"description": "Text snippet from the line to insert after. Uses fuzzy matching. If omitted or not found, appends at end.",
			},
		},
		"required":             []string{"content"},
		"additionalProperties": false,
	}
}

const removeLineDescription = `Remove a line from the plan document.

The line to remove is found by fuzzy matching the 'match' text against existing lines.
The best matching line will be removed.`

func removeLineSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"match": map[string]interface{}{
				"type":        "string",
				"description": "Text snippet to match the line to remove. Uses fuzzy matching to find the best matching line.",
			},
		},
		"required":             []string{"match"},
		"additionalProperties": false,
	}
}

// FindBestMatch finds the line index that best matches the given text.
// Returns -1 if no reasonable match is found.
func FindBestMatch(lines []string, searchText string) int {
	if searchText == "" || len(lines) == 0 {
		return -1
	}

	searchLower := strings.ToLower(strings.TrimSpace(searchText))
	bestIdx := -1
	bestScore := 0

	for i, line := range lines {
		lineLower := strings.ToLower(strings.TrimSpace(line))

		// Exact match (case-insensitive)
		if lineLower == searchLower {
			return i
		}

		// Contains match - prefer lines that contain the search text
		if strings.Contains(lineLower, searchLower) {
			// Score based on how much of the line is covered by the match
			score := len(searchLower) * 100 / max(len(lineLower), 1)
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
			continue
		}

		// Reverse contains - search text contains the line
		if strings.Contains(searchLower, lineLower) && len(lineLower) > 3 {
			score := len(lineLower) * 80 / max(len(searchLower), 1)
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
			continue
		}

		// Word overlap scoring
		searchWords := strings.Fields(searchLower)
		lineWords := strings.Fields(lineLower)
		if len(searchWords) > 0 && len(lineWords) > 0 {
			matches := 0
			for _, sw := range searchWords {
				for _, lw := range lineWords {
					if sw == lw || strings.Contains(lw, sw) || strings.Contains(sw, lw) {
						matches++
						break
					}
				}
			}
			if matches > 0 {
				score := matches * 50 / max(len(searchWords), len(lineWords))
				if score > bestScore {
					bestScore = score
					bestIdx = i
				}
			}
		}
	}

	// Require minimum score for a match
	if bestScore < 20 {
		return -1
	}

	return bestIdx
}
