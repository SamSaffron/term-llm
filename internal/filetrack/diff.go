package filetrack

import (
	"regexp"
	"strconv"
	"strings"

	diff "github.com/shogoki/gotextdiff"
)

// DiffLine is one row of a structured diff hunk.
type DiffLine struct {
	T string `json:"t"` // "ctx" | "add" | "del"
	S string `json:"s"` // line text without the diff prefix
}

// Hunk is one contiguous block of a structured diff.
type Hunk struct {
	OldStart int        `json:"old_start"`
	NewStart int        `json:"new_start"`
	Lines    []DiffLine `json:"lines"`
}

// hunkHeaderRe parses "@@ -start,count +start,count @@" headers
// (same shape as internal/ui/unified_diff.go).
var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// BuildHunks computes a structured diff between two file contents.
// Returns nil when the contents are identical.
func BuildHunks(path string, oldContent, newContent []byte) []Hunk {
	return BuildHunksWithContext(path, oldContent, newContent, 3)
}

// BuildHunksWithContext computes a structured diff with at least contextLines
// unchanged lines around each change. The underlying diff library emits three
// lines of context; larger requests extend and merge those hunks using the
// original retained contents.
func BuildHunksWithContext(path string, oldContent, newContent []byte, contextLines int) []Hunk {
	diffBytes := diff.Diff(path, oldContent, path, newContent)
	if len(diffBytes) == 0 {
		return nil
	}

	var hunks []Hunk
	var current *Hunk
	for _, line := range strings.Split(string(diffBytes), "\n") {
		if line == "" || strings.HasPrefix(line, "diff ") ||
			strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			continue
		}

		switch line[0] {
		case '@':
			matches := hunkHeaderRe.FindStringSubmatch(line)
			if matches == nil {
				continue
			}
			oldStart, _ := strconv.Atoi(matches[1])
			newStart, _ := strconv.Atoi(matches[2])
			hunks = append(hunks, Hunk{OldStart: oldStart, NewStart: newStart})
			current = &hunks[len(hunks)-1]
		case '-':
			if current != nil {
				current.Lines = append(current.Lines, DiffLine{T: "del", S: line[1:]})
			}
		case '+':
			if current != nil {
				current.Lines = append(current.Lines, DiffLine{T: "add", S: line[1:]})
			}
		case ' ':
			if current != nil {
				current.Lines = append(current.Lines, DiffLine{T: "ctx", S: line[1:]})
			}
		}
		// '\ No newline at end of file' and anything unknown is skipped.
	}
	if contextLines <= 3 || len(hunks) == 0 || len(oldContent) == 0 || len(newContent) == 0 {
		return hunks
	}
	return expandHunkContext(hunks, contentLines(oldContent), contentLines(newContent), contextLines-3)
}

// LineCount reports the number of displayable lines in retained file content.
func LineCount(content []byte) int {
	return len(contentLines(content))
}

func contentLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	lines := strings.Split(string(content), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func hunkLineCounts(h Hunk) (oldCount, newCount int) {
	for _, line := range h.Lines {
		switch line.T {
		case "add":
			newCount++
		case "del":
			oldCount++
		default:
			oldCount++
			newCount++
		}
	}
	return oldCount, newCount
}

func expandHunkContext(hunks []Hunk, oldLines, newLines []string, extra int) []Hunk {
	expanded := make([]Hunk, 0, len(hunks))
	var expandedOldCount, expandedNewCount int
	for index, original := range hunks {
		h := Hunk{OldStart: original.OldStart, NewStart: original.NewStart, Lines: append([]DiffLine(nil), original.Lines...)}
		oldCount, newCount := hunkLineCounts(h)
		oldBefore := max(0, h.OldStart-1)
		newBefore := max(0, h.NewStart-1)
		if index > 0 {
			previousOldCount, previousNewCount := hunkLineCounts(hunks[index-1])
			oldBefore = max(0, h.OldStart-1-(hunks[index-1].OldStart-1+previousOldCount))
			newBefore = max(0, h.NewStart-1-(hunks[index-1].NewStart-1+previousNewCount))
		}
		before := min(extra, oldBefore, newBefore)
		if before > 0 {
			start := h.OldStart - 1 - before
			prefix := make([]DiffLine, 0, before+len(h.Lines))
			for _, line := range oldLines[start : start+before] {
				prefix = append(prefix, DiffLine{T: "ctx", S: line})
			}
			h.Lines = append(prefix, h.Lines...)
			h.OldStart -= before
			h.NewStart -= before
			oldCount += before
			newCount += before
		}

		oldAfter := max(0, len(oldLines)-(h.OldStart-1+oldCount))
		newAfter := max(0, len(newLines)-(h.NewStart-1+newCount))
		if index+1 < len(hunks) {
			oldAfter = max(0, hunks[index+1].OldStart-1-(original.OldStart-1+oldCount-before))
			newAfter = max(0, hunks[index+1].NewStart-1-(original.NewStart-1+newCount-before))
		}
		after := min(extra, oldAfter, newAfter)
		for _, line := range oldLines[h.OldStart-1+oldCount : h.OldStart-1+oldCount+after] {
			h.Lines = append(h.Lines, DiffLine{T: "ctx", S: line})
		}
		oldCount += after
		newCount += after

		if len(expanded) == 0 {
			expanded = append(expanded, h)
			expandedOldCount = oldCount
			expandedNewCount = newCount
			continue
		}
		previous := &expanded[len(expanded)-1]
		overlapOld := previous.OldStart - 1 + expandedOldCount - (h.OldStart - 1)
		overlapNew := previous.NewStart - 1 + expandedNewCount - (h.NewStart - 1)
		if overlapOld < 0 || overlapOld != overlapNew {
			expanded = append(expanded, h)
			expandedOldCount = oldCount
			expandedNewCount = newCount
			continue
		}
		overlap := min(overlapOld, len(h.Lines))
		previous.Lines = append(previous.Lines, h.Lines[overlap:]...)
		expandedOldCount += oldCount - overlap
		expandedNewCount += newCount - overlap
	}
	return expanded
}

// CountAddsDels counts added and removed lines between two contents.
// Empty/nil sides are treated as a missing file (pure create/delete).
func CountAddsDels(oldContent, newContent []byte) (adds, dels int) {
	if len(oldContent) == 0 && len(newContent) == 0 {
		return 0, 0
	}
	if len(oldContent) == 0 {
		return countLines(newContent), 0
	}
	if len(newContent) == 0 {
		return 0, countLines(oldContent)
	}

	for _, hunk := range BuildHunks("file", oldContent, newContent) {
		for _, line := range hunk.Lines {
			switch line.T {
			case "add":
				adds++
			case "del":
				dels++
			}
		}
	}
	return adds, dels
}

func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	n := strings.Count(string(content), "\n")
	if content[len(content)-1] != '\n' {
		n++
	}
	return n
}
