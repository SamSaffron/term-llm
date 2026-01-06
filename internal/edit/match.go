// Package edit provides streaming edit parsing and application.
package edit

import (
	"fmt"
	"strings"
)

// MatchLevel indicates which matching strategy succeeded.
type MatchLevel int

const (
	MatchExact        MatchLevel = iota // Direct string match
	MatchStripped                       // Whitespace-normalized match
	MatchNonContiguous                  // Match with ... elision markers
	MatchFuzzy                          // Levenshtein similarity match
)

func (m MatchLevel) String() string {
	switch m {
	case MatchExact:
		return "exact"
	case MatchStripped:
		return "stripped"
	case MatchNonContiguous:
		return "non-contiguous"
	case MatchFuzzy:
		return "fuzzy"
	default:
		return "unknown"
	}
}

// MatchResult contains the result of a successful match.
type MatchResult struct {
	Level    MatchLevel // Which matching strategy succeeded
	Start    int        // Byte offset in content (inclusive)
	End      int        // Byte offset in content (exclusive)
	Original string     // The actual matched text from content
}

// elisionMarker is the token used for non-contiguous matching.
const elisionMarker = "..."

// similarityThreshold is the minimum similarity for fuzzy matching.
const similarityThreshold = 0.8

// FindMatch attempts to find the search string in content using multiple strategies.
// It tries in order: exact, stripped, non-contiguous (if ... present), fuzzy.
func FindMatch(content, search string) (MatchResult, error) {
	if search == "" {
		return MatchResult{}, fmt.Errorf("search string is empty")
	}

	// Level 1: Exact match
	if idx := strings.Index(content, search); idx >= 0 {
		return MatchResult{
			Level:    MatchExact,
			Start:    idx,
			End:      idx + len(search),
			Original: search,
		}, nil
	}

	// Level 2: Stripped (whitespace-normalized per line)
	if result, ok := findStrippedMatch(content, search); ok {
		return result, nil
	}

	// Level 3: Non-contiguous (with ... markers)
	if strings.Contains(search, elisionMarker) {
		if result, ok := findNonContiguousMatch(content, search); ok {
			return result, nil
		}
	}

	// Level 4: Fuzzy (Levenshtein similarity)
	if result, ok := findFuzzyMatch(content, search); ok {
		return result, nil
	}

	return MatchResult{}, fmt.Errorf("search not found: %s", truncateForError(search, 80))
}

// FindMatchWithGuard searches only within a guarded line range.
// startLine and endLine are 1-indexed, inclusive.
func FindMatchWithGuard(content, search string, startLine, endLine int) (MatchResult, error) {
	if search == "" {
		return MatchResult{}, fmt.Errorf("search string is empty")
	}

	guardStart, guardEnd := lineRangeToByteRange(content, startLine, endLine)
	guardedContent := content[guardStart:guardEnd]

	result, err := FindMatch(guardedContent, search)
	if err != nil {
		return MatchResult{}, fmt.Errorf("not found within lines %d-%d: %w", startLine, endLine, err)
	}

	// Adjust offsets back to full content
	result.Start += guardStart
	result.End += guardStart
	return result, nil
}

// ApplyMatch replaces the matched region with newString.
func ApplyMatch(content string, match MatchResult, newString string) string {
	return content[:match.Start] + newString + content[match.End:]
}

// findStrippedMatch tries to match with whitespace normalization.
func findStrippedMatch(content, search string) (MatchResult, bool) {
	contentLines := strings.Split(content, "\n")
	searchLines := strings.Split(search, "\n")

	if len(searchLines) == 0 {
		return MatchResult{}, false
	}

	// Trim search lines
	trimmedSearch := make([]string, len(searchLines))
	for i, line := range searchLines {
		trimmedSearch[i] = strings.TrimSpace(line)
	}

	// Try to find matching sequence
	for i := 0; i <= len(contentLines)-len(searchLines); i++ {
		match := true
		for j := 0; j < len(searchLines); j++ {
			if strings.TrimSpace(contentLines[i+j]) != trimmedSearch[j] {
				match = false
				break
			}
		}
		if match {
			// Calculate byte offsets
			start := 0
			for k := 0; k < i; k++ {
				start += len(contentLines[k]) + 1 // +1 for newline
			}
			end := start
			for k := 0; k < len(searchLines); k++ {
				end += len(contentLines[i+k])
				if i+k < len(contentLines)-1 {
					end++ // newline
				}
			}
			// Handle trailing newline in search
			if strings.HasSuffix(search, "\n") && end < len(content) {
				end++
			}

			return MatchResult{
				Level:    MatchStripped,
				Start:    start,
				End:      end,
				Original: content[start:end],
			}, true
		}
	}

	return MatchResult{}, false
}

// findNonContiguousMatch handles ... elision markers.
func findNonContiguousMatch(content, search string) (MatchResult, bool) {
	segments := strings.Split(search, elisionMarker)

	// Filter empty segments and trim
	var anchors []string
	for _, seg := range segments {
		trimmed := strings.TrimSpace(seg)
		if trimmed != "" {
			anchors = append(anchors, seg)
		}
	}

	if len(anchors) < 2 {
		return MatchResult{}, false
	}

	start := -1
	end := 0
	searchFrom := 0

	for i, anchor := range anchors {
		// Try exact match first
		idx := strings.Index(content[searchFrom:], anchor)
		if idx < 0 {
			// Try stripped match
			idx = findStrippedAnchor(content[searchFrom:], anchor)
			if idx < 0 {
				return MatchResult{}, false
			}
		}

		absIdx := searchFrom + idx
		if i == 0 {
			start = absIdx
		}

		// Move past this anchor
		searchFrom = absIdx + len(anchor)
		end = searchFrom
	}

	return MatchResult{
		Level:    MatchNonContiguous,
		Start:    start,
		End:      end,
		Original: content[start:end],
	}, true
}

// findStrippedAnchor finds an anchor with whitespace tolerance.
func findStrippedAnchor(content, anchor string) int {
	anchorLines := strings.Split(anchor, "\n")
	contentLines := strings.Split(content, "\n")

	if len(anchorLines) == 0 {
		return -1
	}

	// Single line: just find trimmed
	if len(anchorLines) == 1 {
		trimmedAnchor := strings.TrimSpace(anchor)
		for i, line := range contentLines {
			if strings.Contains(strings.TrimSpace(line), trimmedAnchor) {
				// Calculate byte offset
				offset := 0
				for j := 0; j < i; j++ {
					offset += len(contentLines[j]) + 1
				}
				return offset
			}
		}
		return -1
	}

	// Multi-line: match sequence
	trimmedAnchor := make([]string, len(anchorLines))
	for i, line := range anchorLines {
		trimmedAnchor[i] = strings.TrimSpace(line)
	}

	for i := 0; i <= len(contentLines)-len(anchorLines); i++ {
		match := true
		for j := 0; j < len(anchorLines); j++ {
			if strings.TrimSpace(contentLines[i+j]) != trimmedAnchor[j] {
				match = false
				break
			}
		}
		if match {
			offset := 0
			for k := 0; k < i; k++ {
				offset += len(contentLines[k]) + 1
			}
			return offset
		}
	}

	return -1
}

// findFuzzyMatch uses Levenshtein distance for approximate matching.
func findFuzzyMatch(content, search string) (MatchResult, bool) {
	searchLines := strings.Split(search, "\n")
	contentLines := strings.Split(content, "\n")

	if len(searchLines) == 0 || len(contentLines) < len(searchLines) {
		return MatchResult{}, false
	}

	for i := 0; i <= len(contentLines)-len(searchLines); i++ {
		avgSim := 0.0
		allAboveThreshold := true

		for j := 0; j < len(searchLines); j++ {
			sim := lineSimilarity(contentLines[i+j], searchLines[j])
			if sim < similarityThreshold {
				allAboveThreshold = false
				break
			}
			avgSim += sim
		}

		if allAboveThreshold {
			avgSim /= float64(len(searchLines))
			if avgSim >= similarityThreshold {
				// Calculate byte offsets
				start := 0
				for k := 0; k < i; k++ {
					start += len(contentLines[k]) + 1
				}
				end := start
				for k := 0; k < len(searchLines); k++ {
					end += len(contentLines[i+k])
					if i+k < len(contentLines)-1 {
						end++
					}
				}
				if strings.HasSuffix(search, "\n") && end < len(content) {
					end++
				}

				return MatchResult{
					Level:    MatchFuzzy,
					Start:    start,
					End:      end,
					Original: content[start:end],
				}, true
			}
		}
	}

	return MatchResult{}, false
}

// lineSimilarity computes similarity ratio between two strings (0.0 to 1.0).
func lineSimilarity(a, b string) float64 {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)

	if a == b {
		return 1.0
	}

	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1.0
	}

	dist := levenshteinDistance(a, b)
	return 1.0 - float64(dist)/float64(maxLen)
}

// levenshteinDistance computes edit distance between two strings.
func levenshteinDistance(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)

	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(
				prev[j]+1,
				curr[j-1]+1,
				prev[j-1]+cost,
			)
		}
		prev, curr = curr, prev
	}

	return prev[len(b)]
}

// lineRangeToByteRange converts 1-indexed line numbers to byte offsets.
func lineRangeToByteRange(content string, startLine, endLine int) (int, int) {
	lines := strings.Split(content, "\n")

	startOffset := 0
	endOffset := len(content)

	// Calculate start offset
	if startLine > 1 {
		lineCount := 0
		for i, ch := range content {
			if ch == '\n' {
				lineCount++
				if lineCount == startLine-1 {
					startOffset = i + 1
					break
				}
			}
		}
	}

	// Calculate end offset
	if endLine > 0 && endLine < len(lines) {
		lineCount := 0
		for i, ch := range content {
			if ch == '\n' {
				lineCount++
				if lineCount == endLine {
					endOffset = i + 1
					break
				}
			}
		}
	}

	return startOffset, endOffset
}

// truncateForError truncates a string for error messages.
func truncateForError(s string, maxLen int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 3 {
		first := strings.TrimSpace(lines[0])
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" && len(lines) > 1 {
			last = strings.TrimSpace(lines[len(lines)-2])
		}
		return fmt.Sprintf("%s ... %s", truncateString(first, 40), truncateString(last, 40))
	}

	result := strings.TrimSpace(s)
	return truncateString(result, maxLen)
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func min(nums ...int) int {
	m := nums[0]
	for _, n := range nums[1:] {
		if n < m {
			m = n
		}
	}
	return m
}
