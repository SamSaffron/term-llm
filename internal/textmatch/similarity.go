package textmatch

import (
	"fmt"
	"strings"
	"unicode"
)

// Slug converts a display string to a lowercase filesystem-safe identifier.
func Slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.' || unicode.IsSpace(r):
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// NumberedLineRange extracts a one-indexed inclusive line range and prefixes line numbers.
func NumberedLineRange(content string, startLine, endLine int) string {
	lines := strings.Split(content, "\n")
	start := max(startLine-1, 0)
	end := endLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start >= len(lines) {
		return ""
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i+1, lines[i])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// LineSimilarity returns the normalized byte-based Levenshtein similarity of
// two lines after trimming surrounding whitespace.
func LineSimilarity(a, b string) float64 {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == b {
		return 1
	}
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(LevenshteinDistance(a, b))/float64(maxLen)
}

// LevenshteinDistance computes the byte-based edit distance between two strings.
func LevenshteinDistance(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
