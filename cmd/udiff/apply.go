package udiff

import (
	"fmt"
	"strings"
)

// Apply applies the hunks to the given content and returns the modified content.
func Apply(content string, hunks []Hunk) (string, error) {
	lines := strings.Split(content, "\n")

	// Apply hunks in order
	for i, hunk := range hunks {
		var err error
		lines, err = applyHunk(lines, hunk)
		if err != nil {
			return "", fmt.Errorf("hunk %d: %w", i+1, err)
		}
	}

	return strings.Join(lines, "\n"), nil
}

// ApplyFileDiffs applies multiple file diffs to a map of file contents.
// Returns a new map with the modified contents.
func ApplyFileDiffs(files map[string]string, diffs []FileDiff) (map[string]string, error) {
	result := make(map[string]string)

	// Copy all existing files
	for path, content := range files {
		result[path] = content
	}

	// Apply each diff
	for _, diff := range diffs {
		content, ok := result[diff.Path]
		if !ok {
			return nil, fmt.Errorf("file not found: %s", diff.Path)
		}

		modified, err := Apply(content, diff.Hunks)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", diff.Path, err)
		}

		result[diff.Path] = modified
	}

	return result, nil
}

// applyHunk applies a single hunk to the content lines.
func applyHunk(lines []string, hunk Hunk) ([]string, error) {
	// Find the starting position using the @@ context header
	startPos := 0
	if hunk.Context != "" {
		pos := findContext(lines, hunk.Context, 0)
		if pos < 0 {
			return nil, fmt.Errorf("context not found: %q", hunk.Context)
		}
		startPos = pos
	}

	// Build old and new line sequences from the hunk
	oldSeq, newSeq, hasElision, elisionEndAnchor := extractSequences(hunk.Lines)

	// Find where to apply the change
	matchStart, matchEnd, err := findMatch(lines, oldSeq, startPos, hasElision, elisionEndAnchor)
	if err != nil {
		return nil, err
	}

	// Build the result
	result := make([]string, 0, len(lines)-matchEnd+matchStart+len(newSeq))
	result = append(result, lines[:matchStart]...)
	result = append(result, newSeq...)
	result = append(result, lines[matchEnd:]...)

	return result, nil
}

// extractSequences extracts the old and new line sequences from hunk lines.
// Returns (oldLines, newLines, hasElision, elisionEndAnchor).
// The elisionEndAnchor is the line that follows the elision marker.
// When elision is present, oldLines contains only lines BEFORE the elision.
func extractSequences(hunkLines []Line) (old, new []string, hasElision bool, elisionEndAnchor string) {
	elisionIdx := -1

	// First pass: find if there's an elision and where
	for i, line := range hunkLines {
		if line.Type == Elision {
			hasElision = true
			elisionIdx = i
			break
		}
	}

	if hasElision {
		// Collect old lines (Remove + Context) BEFORE elision
		for i := 0; i < elisionIdx; i++ {
			line := hunkLines[i]
			if line.Type == Remove || line.Type == Context {
				old = append(old, line.Content)
			}
		}

		// Find the end anchor (first Remove or Context AFTER elision)
		for i := elisionIdx + 1; i < len(hunkLines); i++ {
			if hunkLines[i].Type == Remove || hunkLines[i].Type == Context {
				elisionEndAnchor = hunkLines[i].Content
				break
			}
		}

		// Collect all Add lines for the new content
		for _, line := range hunkLines {
			if line.Type == Add {
				new = append(new, line.Content)
			}
		}
	} else {
		// No elision - collect normally
		for _, line := range hunkLines {
			switch line.Type {
			case Context:
				old = append(old, line.Content)
				new = append(new, line.Content)
			case Remove:
				old = append(old, line.Content)
			case Add:
				new = append(new, line.Content)
			}
		}
	}
	return
}

// findContext finds a line containing the context string, starting from pos.
// Returns -1 if not found.
func findContext(lines []string, context string, pos int) int {
	context = strings.TrimSpace(context)
	for i := pos; i < len(lines); i++ {
		if strings.Contains(lines[i], context) {
			return i
		}
	}
	// Try fuzzy match (trimmed)
	for i := pos; i < len(lines); i++ {
		if strings.Contains(strings.TrimSpace(lines[i]), context) {
			return i
		}
	}
	return -1
}

// findMatch finds where the old sequence matches in the content.
// Returns (startIndex, endIndex, error).
// With elision, it finds the start anchor and end anchor with brace tracking.
func findMatch(lines []string, oldSeq []string, startPos int, hasElision bool, elisionEndAnchor string) (int, int, error) {
	if len(oldSeq) == 0 {
		// Pure addition - insert at startPos
		return startPos, startPos, nil
	}

	if hasElision {
		return findMatchWithElision(lines, oldSeq, startPos, elisionEndAnchor)
	}

	return findExactMatch(lines, oldSeq, startPos)
}

// findExactMatch finds an exact sequence match (with fuzzy fallback).
func findExactMatch(lines []string, oldSeq []string, startPos int) (int, int, error) {
	// Try exact match first
	for i := startPos; i <= len(lines)-len(oldSeq); i++ {
		if matchSequence(lines[i:], oldSeq, false) {
			return i, i + len(oldSeq), nil
		}
	}

	// Try fuzzy match (trimmed whitespace)
	for i := startPos; i <= len(lines)-len(oldSeq); i++ {
		if matchSequence(lines[i:], oldSeq, true) {
			return i, i + len(oldSeq), nil
		}
	}

	return 0, 0, fmt.Errorf("could not find matching lines:\n%s", strings.Join(oldSeq, "\n"))
}

// findMatchWithElision finds a match where elision is used.
// It matches the start anchor, then uses brace tracking to find the end.
func findMatchWithElision(lines []string, oldSeq []string, startPos int, endAnchor string) (int, int, error) {
	// Find lines before elision (the start anchors)
	var startAnchors []string
	for _, s := range oldSeq {
		if s == "" && len(startAnchors) == 0 {
			continue // Skip leading empty
		}
		startAnchors = append(startAnchors, s)
	}

	if len(startAnchors) == 0 {
		return 0, 0, fmt.Errorf("elision requires at least one start anchor line")
	}

	// Find the start anchor
	matchStart := -1
	for i := startPos; i <= len(lines)-len(startAnchors); i++ {
		if matchSequence(lines[i:], startAnchors, false) {
			matchStart = i
			break
		}
	}
	if matchStart < 0 {
		// Try fuzzy
		for i := startPos; i <= len(lines)-len(startAnchors); i++ {
			if matchSequence(lines[i:], startAnchors, true) {
				matchStart = i
				break
			}
		}
	}
	if matchStart < 0 {
		return 0, 0, fmt.Errorf("could not find start anchor: %q", startAnchors[0])
	}

	// Find the end position using brace tracking
	searchStart := matchStart + len(startAnchors)
	endPos, err := findElisionEnd(lines, searchStart, endAnchor)
	if err != nil {
		return 0, 0, err
	}

	return matchStart, endPos, nil
}

// findElisionEnd finds where the elision ends by matching the end anchor.
// Uses brace depth tracking to handle nested braces correctly.
func findElisionEnd(lines []string, startPos int, endAnchor string) (int, error) {
	endAnchor = strings.TrimSpace(endAnchor)

	// Track brace depth to find the matching closing brace
	depth := 0

	// Count initial depth from context (assume we're inside one level)
	depth = 1

	for i := startPos; i < len(lines); i++ {
		line := lines[i]

		// Update brace depth (respecting strings and comments)
		depth = updateBraceDepth(depth, line)

		// Check if this line matches the end anchor at the right depth
		trimmed := strings.TrimSpace(line)
		if depth == 0 && (trimmed == endAnchor || strings.HasPrefix(trimmed, endAnchor)) {
			return i + 1, nil // End is exclusive
		}

		// Also check if we hit depth 0 and the anchor is just "}"
		if depth == 0 && endAnchor == "}" {
			return i + 1, nil
		}
	}

	return 0, fmt.Errorf("could not find end anchor: %q (brace depth never returned to 0)", endAnchor)
}

// updateBraceDepth updates the brace depth for a line, respecting strings and comments.
func updateBraceDepth(depth int, line string) int {
	inString := false
	inRawString := false
	stringChar := byte(0)
	i := 0

	for i < len(line) {
		ch := line[i]

		// Handle escape sequences in strings
		if (inString || inRawString) && ch == '\\' && i+1 < len(line) {
			i += 2
			continue
		}

		// Handle string literals
		if !inString && !inRawString {
			if ch == '"' {
				inString = true
				stringChar = '"'
				i++
				continue
			}
			if ch == '\'' {
				inString = true
				stringChar = '\''
				i++
				continue
			}
			if ch == '`' {
				inRawString = true
				i++
				continue
			}
		} else if inString && ch == stringChar {
			inString = false
			i++
			continue
		} else if inRawString && ch == '`' {
			inRawString = false
			i++
			continue
		}

		// Handle line comments
		if !inString && !inRawString && ch == '/' && i+1 < len(line) && line[i+1] == '/' {
			// Rest of line is comment
			break
		}

		// Handle block comment start (simplified - doesn't track multi-line)
		if !inString && !inRawString && ch == '/' && i+1 < len(line) && line[i+1] == '*' {
			// Skip to end of block comment on same line
			end := strings.Index(line[i+2:], "*/")
			if end >= 0 {
				i = i + 2 + end + 2
				continue
			}
			// Block comment continues to next line - skip rest
			break
		}

		// Count braces
		if !inString && !inRawString {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
			}
		}

		i++
	}

	return depth
}

// matchSequence checks if the lines starting at content match the pattern.
func matchSequence(content []string, pattern []string, fuzzy bool) bool {
	if len(content) < len(pattern) {
		return false
	}

	for i, p := range pattern {
		if fuzzy {
			if strings.TrimSpace(content[i]) != strings.TrimSpace(p) {
				return false
			}
		} else {
			if content[i] != p {
				return false
			}
		}
	}

	return true
}
