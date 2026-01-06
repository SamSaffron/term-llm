package edit

import (
	"fmt"
	"strings"
)

// RetryContext contains context for building a retry prompt after a failed edit.
type RetryContext struct {
	FilePath      string   // Path to the file that failed
	FailedSearch  string   // The search string that didn't match (search/replace)
	DiffLines     []string // The diff lines that failed (unified diff)
	FileContent   string   // Current content of the file
	Reason        string   // Why the edit failed
	PartialOutput string   // What the LLM output before failure
	AttemptNumber int      // Which retry attempt this is (0 = first try)
}

// BuildRetryPrompt creates a prompt to help the LLM retry after a failed edit.
func BuildRetryPrompt(ctx RetryContext) string {
	var sb strings.Builder

	sb.WriteString("Edit failed. Please retry with corrected content.\n\n")

	sb.WriteString(fmt.Sprintf("**File:** %s\n\n", ctx.FilePath))
	sb.WriteString(fmt.Sprintf("**Error:** %s\n\n", ctx.Reason))

	if ctx.FailedSearch != "" {
		sb.WriteString("**Your search block:**\n```\n")
		sb.WriteString(ctx.FailedSearch)
		if !strings.HasSuffix(ctx.FailedSearch, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
	}

	if len(ctx.DiffLines) > 0 {
		sb.WriteString("**Your diff:**\n```\n")
		for _, line := range ctx.DiffLines {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
	}

	// Show relevant portion of the file
	if ctx.FileContent != "" {
		nearby := findNearbyContent(ctx.FileContent, ctx.FailedSearch, 15)
		if nearby != "" {
			sb.WriteString("**Relevant file content:**\n```\n")
			sb.WriteString(nearby)
			if !strings.HasSuffix(nearby, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n\n")
		}
	}

	sb.WriteString("Please provide the edit again with the EXACT text from the file.\n")
	sb.WriteString("Copy the text character-for-character, including whitespace and indentation.\n")

	return sb.String()
}

// findNearbyContent finds the portion of the file most relevant to the failed search.
func findNearbyContent(content, search string, contextLines int) string {
	if search == "" {
		// No search, return first portion of file
		return extractLines(content, 1, contextLines*2)
	}

	lines := strings.Split(content, "\n")
	searchLines := strings.Split(search, "\n")

	if len(searchLines) == 0 {
		return extractLines(content, 1, contextLines*2)
	}

	// Find best matching line
	firstSearchLine := strings.TrimSpace(searchLines[0])
	bestMatchIdx := -1
	bestMatchScore := 0.0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Exact substring match
		if strings.Contains(trimmed, firstSearchLine) || strings.Contains(firstSearchLine, trimmed) {
			bestMatchIdx = i
			bestMatchScore = 1.0
			break
		}

		// Similarity match
		sim := lineSimilarity(trimmed, firstSearchLine)
		if sim > bestMatchScore && sim > 0.5 {
			bestMatchScore = sim
			bestMatchIdx = i
		}
	}

	if bestMatchIdx < 0 {
		// No good match found, return first portion
		return extractLines(content, 1, contextLines*2)
	}

	// Extract lines around the best match
	startLine := bestMatchIdx - contextLines
	if startLine < 0 {
		startLine = 0
	}
	endLine := bestMatchIdx + contextLines
	if endLine > len(lines) {
		endLine = len(lines)
	}

	return extractLinesWithNumbers(lines, startLine, endLine)
}

// extractLines extracts lines startLine to endLine (1-indexed) from content.
func extractLines(content string, startLine, endLine int) string {
	lines := strings.Split(content, "\n")
	return extractLinesWithNumbers(lines, startLine-1, endLine)
}

// extractLinesWithNumbers extracts lines with line numbers (0-indexed input).
func extractLinesWithNumbers(lines []string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return ""
	}

	var sb strings.Builder
	for i := start; i < end; i++ {
		sb.WriteString(fmt.Sprintf("%4d: %s\n", i+1, lines[i]))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// FindClosestLines finds the lines in content most similar to the search.
// Returns the line numbers and content of the closest matches.
func FindClosestLines(content, search string, maxResults int) []ClosestLine {
	if search == "" {
		return nil
	}

	lines := strings.Split(content, "\n")
	searchLines := strings.Split(search, "\n")

	if len(searchLines) == 0 {
		return nil
	}

	firstSearchLine := strings.TrimSpace(searchLines[0])
	if firstSearchLine == "" && len(searchLines) > 1 {
		firstSearchLine = strings.TrimSpace(searchLines[1])
	}

	type scored struct {
		lineNum int
		line    string
		score   float64
	}

	var candidates []scored
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		sim := lineSimilarity(trimmed, firstSearchLine)
		if sim > 0.3 {
			candidates = append(candidates, scored{
				lineNum: i + 1,
				line:    line,
				score:   sim,
			})
		}
	}

	// Sort by score descending
	for i := 0; i < len(candidates)-1; i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	// Take top results
	if len(candidates) > maxResults {
		candidates = candidates[:maxResults]
	}

	result := make([]ClosestLine, len(candidates))
	for i, c := range candidates {
		result[i] = ClosestLine{
			LineNum:    c.lineNum,
			Content:    c.line,
			Similarity: c.score,
		}
	}

	return result
}

// ClosestLine represents a line from the file that closely matches the search.
type ClosestLine struct {
	LineNum    int
	Content    string
	Similarity float64
}
