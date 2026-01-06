package ui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	diff "github.com/shogoki/gotextdiff"
)

// PrintUnifiedDiff prints a clean unified diff between old and new content
// If multiFile is true, shows the filename header (for multi-file diffs)
func PrintUnifiedDiff(filePath, oldContent, newContent string) {
	printUnifiedDiffInternal(filePath, oldContent, newContent, false)
}

// PrintUnifiedDiffMulti prints a diff with filename header (for multi-file edits)
func PrintUnifiedDiffMulti(filePath, oldContent, newContent string) {
	printUnifiedDiffInternal(filePath, oldContent, newContent, true)
}

func printUnifiedDiffInternal(filePath, oldContent, newContent string, multiFile bool) {
	if oldContent == newContent {
		return
	}

	styles := DefaultStyles()

	// Print header
	fmt.Printf("%s %s\n", styles.Bold.Render("Edit:"), filePath)

	// Generate unified diff using gotextdiff
	diffBytes := diff.Diff(filePath, []byte(oldContent), filePath, []byte(newContent))
	if len(diffBytes) == 0 {
		return
	}

	diffText := string(diffBytes)

	// Create highlighter for syntax highlighting
	highlighter := NewHighlighter(filePath)

	// Calculate line number width based on file sizes
	oldLines := strings.Count(oldContent, "\n") + 1
	newLines := strings.Count(newContent, "\n") + 1
	maxLine := oldLines
	if newLines > maxLine {
		maxLine = newLines
	}
	lineNumWidth := len(strconv.Itoa(maxLine))
	if lineNumWidth < 3 {
		lineNumWidth = 3
	}

	// Track current line numbers
	var oldLineNum, newLineNum int
	hunkCount := 0

	// Regex to parse hunk header: @@ -start,count +start,count @@
	hunkRe := regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

	// Parse and colorize the diff
	lines := strings.Split(diffText, "\n")
	for _, line := range lines {
		// Skip the "diff" line and --- / +++ headers
		if strings.HasPrefix(line, "diff ") ||
			strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "+++ ") {
			continue
		}

		if len(line) == 0 {
			continue
		}

		prefix := line[0]
		content := ""
		if len(line) > 1 {
			content = line[1:]
		}

		switch prefix {
		case '@':
			// Parse hunk header to get starting line numbers
			if matches := hunkRe.FindStringSubmatch(line); matches != nil {
				oldLineNum, _ = strconv.Atoi(matches[1])
				newLineNum, _ = strconv.Atoi(matches[2])
			}
			// Show "..." separator between hunks (not before first one)
			if hunkCount > 0 {
				fmt.Printf("\x1b[38;2;100;100;100m%s\x1b[0m\n", strings.Repeat(" ", lineNumWidth)+"  ...")
			}
			hunkCount++

		case '-':
			// Removed line - red background with old line number
			highlighted := content
			if highlighter != nil {
				highlighted = highlighter.HighlightLineWithBg(content, diffRemoveBg)
			} else {
				highlighted = fmt.Sprintf("\x1b[48;2;%d;%d;%dm%s\x1b[0m", diffRemoveBg[0], diffRemoveBg[1], diffRemoveBg[2], content)
			}
			fmt.Printf("\x1b[38;2;160;80;80m%*d- \x1b[0m%s\n", lineNumWidth, oldLineNum, highlighted)
			oldLineNum++

		case '+':
			// Added line - green background with new line number
			highlighted := content
			if highlighter != nil {
				highlighted = highlighter.HighlightLineWithBg(content, diffAddBg)
			} else {
				highlighted = fmt.Sprintf("\x1b[48;2;%d;%d;%dm%s\x1b[0m", diffAddBg[0], diffAddBg[1], diffAddBg[2], content)
			}
			fmt.Printf("\x1b[38;2;80;160;80m%*d+ \x1b[0m%s\n", lineNumWidth, newLineNum, highlighted)
			newLineNum++

		case ' ':
			// Context line - no background, show line number in grey
			highlighted := content
			if highlighter != nil {
				highlighted = highlighter.HighlightLine(content)
			}
			fmt.Printf("\x1b[38;2;100;100;100m%*d  \x1b[0m%s\n", lineNumWidth, oldLineNum, highlighted)
			oldLineNum++
			newLineNum++

		default:
			// Unknown prefix - just print as-is
			fmt.Println(line)
		}
	}
}

// HasDiff returns true if old and new content are different
func HasDiff(oldContent, newContent string) bool {
	return oldContent != newContent
}
