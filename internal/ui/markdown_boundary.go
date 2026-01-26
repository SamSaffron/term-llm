package ui

import "strings"

// FindSafeBoundary finds the last byte position where markdown context is complete.
// Returns -1 if no safe boundary exists.
//
// A safe boundary is a position after which we can split the text without
// breaking markdown rendering. Safe positions are:
// - After complete paragraphs (\n\n)
// - After closed code blocks (balanced ``` markers)
// - When not inside incomplete inline markers (**, `, etc.)
func FindSafeBoundary(text string) int {
	if len(text) < 20 {
		return -1 // Too short to bother caching
	}

	// Find all paragraph boundaries and check them from last to first
	// We want the latest safe boundary
	pos := len(text)
	for {
		paraEnd := strings.LastIndex(text[:pos], "\n\n")
		if paraEnd == -1 {
			return -1 // No paragraph boundary found
		}

		safePos := paraEnd + 2

		// Skip if inside code block
		if isInCodeBlock(text, safePos) {
			pos = paraEnd
			continue
		}

		// Check if inline markers are balanced up to this point
		if areInlineMarkersBalanced(text[:safePos]) {
			return safePos
		}

		// Try earlier boundary
		pos = paraEnd
	}
}

// FindSafeBoundaryIncremental finds a safe boundary only scanning from lastSafePos.
// This is O(delta) instead of O(n) for streaming scenarios.
// Returns -1 if no new safe boundary exists beyond lastSafePos.
func FindSafeBoundaryIncremental(text string, lastSafePos int) int {
	if len(text) < 20 {
		return -1 // Too short to bother caching
	}

	// Only search in the new portion of text
	if lastSafePos >= len(text) {
		return -1
	}

	// Start searching from lastSafePos
	searchStart := lastSafePos
	if searchStart < 0 {
		searchStart = 0
	}

	// Find paragraph boundaries in the delta region, working backwards from end
	pos := len(text)
	for {
		paraEnd := strings.LastIndex(text[:pos], "\n\n")
		if paraEnd == -1 || paraEnd < searchStart {
			return -1 // No new paragraph boundary found in delta
		}

		safePos := paraEnd + 2

		// Skip if inside code block (this still needs full text context)
		if isInCodeBlockFast(text, safePos) {
			pos = paraEnd
			continue
		}

		// Check if inline markers are balanced up to this point
		if areInlineMarkersBalanced(text[:safePos]) {
			return safePos
		}

		// Try earlier boundary
		pos = paraEnd
	}
}

// isInCodeBlockFast is an optimized version that avoids strings.Split.
// Returns true if position pos is inside an unclosed code block.
func isInCodeBlockFast(text string, pos int) bool {
	if pos > len(text) {
		pos = len(text)
	}

	prefix := text[:pos]
	count := countCodeFencesFast(prefix)

	// Odd number of fences means we're inside a code block
	return count%2 == 1
}

// countCodeFencesFast counts ``` markers without using strings.Split.
func countCodeFencesFast(text string) int {
	count := 0
	lineStart := 0

	for i := 0; i <= len(text); i++ {
		// Check for end of line or end of text
		if i == len(text) || text[i] == '\n' {
			if i > lineStart {
				line := text[lineStart:i]
				// Check if line starts with ``` (after trimming leading whitespace)
				trimmed := strings.TrimLeft(line, " \t")
				if strings.HasPrefix(trimmed, "```") {
					count++
				}
			}
			lineStart = i + 1
		}
	}
	return count
}

// isInCodeBlock returns true if position pos is inside an unclosed code block.
// Code blocks are delimited by ``` at the start of a line.
func isInCodeBlock(text string, pos int) bool {
	if pos > len(text) {
		pos = len(text)
	}

	prefix := text[:pos]
	count := countCodeFences(prefix)

	// Odd number of fences means we're inside a code block
	return count%2 == 1
}

// countCodeFences counts ``` markers that appear at the start of a line.
func countCodeFences(text string) int {
	count := 0
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "```") {
			count++
		}
	}
	return count
}

// areInlineMarkersBalanced checks if common inline markdown markers are balanced.
// This checks for paired **, *, `, ~~ markers outside of code spans.
func areInlineMarkersBalanced(text string) bool {
	// Track if we're inside various inline elements
	inCodeSpan := false
	inBold := false
	inItalicAsterisk := false
	inItalicUnderscore := false
	inStrikethrough := false

	i := 0
	for i < len(text) {
		// Handle code spans first (they escape other markers)
		if !inCodeSpan && i < len(text) && text[i] == '`' {
			// Count consecutive backticks
			start := i
			for i < len(text) && text[i] == '`' {
				i++
			}
			backtickCount := i - start
			// Look for matching closing backticks
			closing := strings.Repeat("`", backtickCount)
			closeIdx := strings.Index(text[i:], closing)
			if closeIdx == -1 {
				return false // Unclosed code span
			}
			i += closeIdx + backtickCount
			continue
		}

		// Check for bold/italic with asterisks
		if text[i] == '*' {
			if i+1 < len(text) && text[i+1] == '*' {
				// ** - bold
				inBold = !inBold
				i += 2
				continue
			}
			// Single * - italic
			inItalicAsterisk = !inItalicAsterisk
			i++
			continue
		}

		// Check for italic with underscore (only at word boundaries)
		if text[i] == '_' {
			// Simplified: just toggle state
			inItalicUnderscore = !inItalicUnderscore
			i++
			continue
		}

		// Check for strikethrough
		if text[i] == '~' && i+1 < len(text) && text[i+1] == '~' {
			inStrikethrough = !inStrikethrough
			i += 2
			continue
		}

		i++
	}

	// All markers should be closed
	return !inCodeSpan && !inBold && !inItalicAsterisk && !inItalicUnderscore && !inStrikethrough
}
