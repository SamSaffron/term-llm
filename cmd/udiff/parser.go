// Package udiff provides parsing and application of unified diffs with elision support.
//
// The format supports:
//   - Standard unified diff headers (--- and +++)
//   - Context headers (@@ func Name @@) for locating code
//   - Elision markers (-...) for matching large blocks without listing every line
//   - Context lines (space prefix), remove lines (-), and add lines (+)
package udiff

import (
	"fmt"
	"strings"
)

// LineType represents the type of a diff line.
type LineType int

const (
	// Context is an unchanged line (space prefix)
	Context LineType = iota
	// Remove is a line to be removed (- prefix)
	Remove
	// Add is a line to be added (+ prefix)
	Add
	// Elision is a marker that matches any content (-...)
	Elision
)

// Line represents a single line in a diff hunk.
type Line struct {
	Type    LineType
	Content string // Content without the prefix
}

// Hunk represents a single change block within a file diff.
type Hunk struct {
	Context string // The @@ header content (e.g., "func ProcessData")
	Lines   []Line // All lines in order
}

// FileDiff represents all changes to a single file.
type FileDiff struct {
	Path  string // File path from --- and +++ headers
	Hunks []Hunk
}

// Parse parses a unified diff string into a slice of FileDiff.
// The format expected is:
//
//	--- path/to/file
//	+++ path/to/file
//	@@ optional context @@
//	 context line
//	-removed line
//	+added line
//	-...
//
// Multiple files can be included in a single diff.
func Parse(diff string) ([]FileDiff, error) {
	lines := strings.Split(diff, "\n")
	var result []FileDiff
	var currentFile *FileDiff
	var currentHunk *Hunk

	i := 0
	for i < len(lines) {
		line := lines[i]

		// Empty lines: if we're inside a hunk, treat as empty context; otherwise skip
		if line == "" {
			if currentHunk != nil {
				currentHunk.Lines = append(currentHunk.Lines, Line{Type: Context, Content: ""})
			}
			i++
			continue
		}

		// File header: --- path
		if strings.HasPrefix(line, "--- ") {
			// Save previous file if exists
			if currentFile != nil {
				if currentHunk != nil {
					trimTrailingEmptyLines(currentHunk)
					if len(currentHunk.Lines) > 0 {
						currentFile.Hunks = append(currentFile.Hunks, *currentHunk)
					}
				}
				result = append(result, *currentFile)
			}

			path := strings.TrimPrefix(line, "--- ")
			// Handle a/path and b/path prefixes from git diff
			path = strings.TrimPrefix(path, "a/")

			currentFile = &FileDiff{Path: path}
			currentHunk = nil
			i++

			// Skip empty lines between --- and +++
			for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}

			// Expect +++ line next
			if i < len(lines) && strings.HasPrefix(lines[i], "+++ ") {
				// Verify paths match (ignoring a/ b/ prefixes)
				plusPath := strings.TrimPrefix(lines[i], "+++ ")
				plusPath = strings.TrimPrefix(plusPath, "b/")
				if plusPath != path {
					// Use the +++ path if different (some diffs have /dev/null for new files)
					if path == "/dev/null" {
						currentFile.Path = plusPath
					}
				}
				i++
			}
			continue
		}

		// Hunk header: @@ context @@ or just @@
		if strings.HasPrefix(line, "@@") {
			// Save previous hunk if exists
			if currentHunk != nil {
				trimTrailingEmptyLines(currentHunk)
				if len(currentHunk.Lines) > 0 && currentFile != nil {
					currentFile.Hunks = append(currentFile.Hunks, *currentHunk)
				}
			}

			context := parseHunkHeader(line)
			currentHunk = &Hunk{Context: context}
			i++
			continue
		}

		// Diff lines within a hunk
		if currentHunk != nil {
			parsed, err := parseDiffLine(line)
			if err != nil {
				// Not a diff line - might be end of hunk or malformed
				// Try to continue parsing
				i++
				continue
			}
			currentHunk.Lines = append(currentHunk.Lines, parsed)
			i++
			continue
		}

		// If we have a file but no hunk yet, and this looks like a diff line,
		// create a hunk without context
		if currentFile != nil && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+")) {
			currentHunk = &Hunk{}
			parsed, err := parseDiffLine(line)
			if err == nil {
				currentHunk.Lines = append(currentHunk.Lines, parsed)
			}
			i++
			continue
		}

		// Unknown line, skip
		i++
	}

	// Save final file and hunk
	if currentFile != nil {
		if currentHunk != nil {
			trimTrailingEmptyLines(currentHunk)
		}
		if currentHunk != nil && len(currentHunk.Lines) > 0 {
			currentFile.Hunks = append(currentFile.Hunks, *currentHunk)
		}
		result = append(result, *currentFile)
	}

	return result, nil
}

// parseHunkHeader extracts the context from a @@ header line.
// Formats supported:
//   - @@ context @@
//   - @@ context
//   - @@
func parseHunkHeader(line string) string {
	line = strings.TrimPrefix(line, "@@")
	line = strings.TrimSpace(line)

	// Remove trailing @@
	if strings.HasSuffix(line, "@@") {
		line = strings.TrimSuffix(line, "@@")
		line = strings.TrimSpace(line)
	}

	return line
}

// parseDiffLine parses a single diff line and returns its type and content.
func parseDiffLine(line string) (Line, error) {
	if len(line) == 0 {
		// Empty line is treated as empty context
		return Line{Type: Context, Content: ""}, nil
	}

	prefix := line[0]
	content := ""
	if len(line) > 1 {
		content = line[1:]
	}

	switch prefix {
	case ' ':
		return Line{Type: Context, Content: content}, nil
	case '-':
		// Check for elision marker
		if content == "..." || strings.TrimSpace(content) == "..." {
			return Line{Type: Elision, Content: ""}, nil
		}
		return Line{Type: Remove, Content: content}, nil
	case '+':
		return Line{Type: Add, Content: content}, nil
	default:
		return Line{}, fmt.Errorf("invalid diff line prefix: %q", prefix)
	}
}

// String returns the line type as a string for debugging.
func (t LineType) String() string {
	switch t {
	case Context:
		return "Context"
	case Remove:
		return "Remove"
	case Add:
		return "Add"
	case Elision:
		return "Elision"
	default:
		return "Unknown"
	}
}

// trimTrailingEmptyLines removes trailing empty context lines from a hunk.
// This handles the common case of trailing newlines in diff strings.
func trimTrailingEmptyLines(h *Hunk) {
	for len(h.Lines) > 0 {
		last := h.Lines[len(h.Lines)-1]
		if last.Type == Context && last.Content == "" {
			h.Lines = h.Lines[:len(h.Lines)-1]
		} else {
			break
		}
	}
}
