package cmd

import (
	"fmt"
	"strings"
)

const editWildcardToken = "<<<elided>>>"

type editMatch struct {
	start int
	end   int
	text  string
}

func findEditMatch(content, oldString string) (editMatch, error) {
	if oldString == "" {
		return editMatch{}, fmt.Errorf("old_string is empty")
	}

	if !strings.Contains(oldString, editWildcardToken) {
		idx := strings.Index(content, oldString)
		if idx < 0 {
			return editMatch{}, fmt.Errorf("old_string not found: %.40s...", oldString)
		}
		return editMatch{
			start: idx,
			end:   idx + len(oldString),
			text:  oldString,
		}, nil
	}

	segments := strings.Split(oldString, editWildcardToken)
	foundSegment := false
	foundFirst := false
	start := 0
	end := 0
	searchFrom := 0

	for _, segment := range segments {
		if segment == "" {
			continue
		}
		foundSegment = true
		idx := strings.Index(content[searchFrom:], segment)
		if idx < 0 {
			return editMatch{}, fmt.Errorf("wildcard match failed: %.40s...", oldString)
		}
		absIdx := searchFrom + idx
		if !foundFirst {
			start = absIdx
			foundFirst = true
		}
		searchFrom = absIdx + len(segment)
		end = searchFrom
	}

	if !foundSegment {
		return editMatch{}, fmt.Errorf("wildcard token used without anchors")
	}

	return editMatch{
		start: start,
		end:   end,
		text:  content[start:end],
	}, nil
}

func applyEditMatch(content string, match editMatch, newString string) string {
	return content[:match.start] + newString + content[match.end:]
}

// lineRangeToByteRange converts 1-indexed line numbers to byte offsets
// Returns (startOffset, endOffset) where endOffset is exclusive
func lineRangeToByteRange(content string, startLine, endLine int) (int, int) {
	lines := strings.Split(content, "\n")

	startOffset := 0
	endOffset := len(content)

	// Calculate start offset (byte position at start of startLine)
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

	// Calculate end offset (byte position after endLine, including its newline)
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

// findEditMatchWithGuard searches for oldString only within the guarded line range
// Returns a match with offsets relative to the full content
func findEditMatchWithGuard(content, oldString string, startLine, endLine int) (editMatch, error) {
	if oldString == "" {
		return editMatch{}, fmt.Errorf("old_string is empty")
	}

	guardStart, guardEnd := lineRangeToByteRange(content, startLine, endLine)
	guardedContent := content[guardStart:guardEnd]

	// Search within the guarded slice
	if !strings.Contains(oldString, editWildcardToken) {
		idx := strings.Index(guardedContent, oldString)
		if idx < 0 {
			return editMatch{}, fmt.Errorf("not found within lines %d-%d", startLine, endLine)
		}
		// Adjust offset back to full content
		absStart := guardStart + idx
		return editMatch{
			start: absStart,
			end:   absStart + len(oldString),
			text:  oldString,
		}, nil
	}

	// Wildcard matching within guarded slice
	segments := strings.Split(oldString, editWildcardToken)
	foundSegment := false
	foundFirst := false
	start := 0
	end := 0
	searchFrom := 0

	for _, segment := range segments {
		if segment == "" {
			continue
		}
		foundSegment = true
		idx := strings.Index(guardedContent[searchFrom:], segment)
		if idx < 0 {
			return editMatch{}, fmt.Errorf("wildcard match failed within lines %d-%d", startLine, endLine)
		}
		absIdx := searchFrom + idx
		if !foundFirst {
			start = absIdx
			foundFirst = true
		}
		searchFrom = absIdx + len(segment)
		end = searchFrom
	}

	if !foundSegment {
		return editMatch{}, fmt.Errorf("wildcard token used without anchors")
	}

	// Adjust offsets back to full content
	return editMatch{
		start: guardStart + start,
		end:   guardStart + end,
		text:  guardedContent[start:end],
	}, nil
}
