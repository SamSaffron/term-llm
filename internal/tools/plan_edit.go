package tools

import (
	"strings"
)

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
