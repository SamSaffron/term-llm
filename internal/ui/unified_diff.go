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

func printUnifiedDiffInternal(filePath, oldContent, newContent string, _ bool) {
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
	var deletionOffset int // Tracks position within a deletion block
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
			// Removed line - red background, show virtual new file position
			highlighted := content
			if highlighter != nil {
				highlighted = highlighter.HighlightLineWithBg(content, diffRemoveBg)
			} else {
				highlighted = fmt.Sprintf("\x1b[48;2;%d;%d;%dm%s\x1b[0m", diffRemoveBg[0], diffRemoveBg[1], diffRemoveBg[2], content)
			}
			fmt.Printf("\x1b[38;2;160;80;80m%*d- \x1b[0m%s\n", lineNumWidth, newLineNum+deletionOffset, highlighted)
			oldLineNum++
			deletionOffset++

		case '+':
			// Added line - green background with new line number
			deletionOffset = 0 // Reset deletion offset when we see additions
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
			deletionOffset = 0 // Reset deletion offset when we see context
			highlighted := content
			if highlighter != nil {
				highlighted = highlighter.HighlightLine(content)
			}
			fmt.Printf("\x1b[38;2;100;100;100m%*d  \x1b[0m%s\n", lineNumWidth, newLineNum, highlighted)
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

// maxDiffLines is the maximum number of diff lines to render inline
const maxDiffLines = 50

// maxDiffContentWidth is the maximum width for diff content before wrapping
const maxDiffContentWidth = 90

// wrapDiffLine wraps a long diff line, returning wrapped lines.
// All lines are padded to contentWidth to create uniform background blocks.
// lineNumWidth is the width of the line number column.
// prefix is the diff prefix ('+', '-', or ' ').
// prefixColor is the ANSI color for the prefix.
// content is the raw line content (will be highlighted internally).
// bgColor is optional background color [R,G,B] for the block (nil for none).
func wrapDiffLine(lineNumWidth int, lineNum int, prefix byte, prefixColor string, content string, contentWidth int, bgColor []int) string {
	// Build background color codes
	bgStart := ""
	if bgColor != nil {
		bgStart = fmt.Sprintf("\x1b[48;2;%d;%d;%dm", bgColor[0], bgColor[1], bgColor[2])
	}

	// Helper to build a complete line with background extending from prefix through content
	buildLine := func(lineNumStr string, chunk string) string {
		// Re-apply background after any resets in the syntax-highlighted content
		if bgStart != "" {
			chunk = strings.ReplaceAll(chunk, "\x1b[0m", "\x1b[0m"+bgStart)
		}
		visibleLen := len(stripAnsi(chunk))
		padding := ""
		if visibleLen < contentWidth {
			padding = strings.Repeat(" ", contentWidth-visibleLen)
		}
		// Format: bg + prefix_color + line_num + prefix + reset_all + bg + content + padding + reset
		// The reset after prefix clears everything, then we re-apply just background for content
		return fmt.Sprintf("%s%s%s%c \x1b[0m%s%s%s\x1b[0m\n", bgStart, prefixColor, lineNumStr, prefix, bgStart, chunk, padding)
	}

	// If content fits, return single padded line
	rawLen := len(stripAnsi(content))
	if rawLen <= contentWidth {
		lineNumStr := fmt.Sprintf("%*d", lineNumWidth, lineNum)
		return buildLine(lineNumStr, content)
	}

	// Need to wrap - split content into chunks
	var b strings.Builder
	remaining := content
	isFirst := true
	activeColors := "" // Track active ANSI codes across chunks

	for len(remaining) > 0 {
		rawRemaining := stripAnsi(remaining)
		if len(rawRemaining) == 0 {
			break
		}

		chunkLen := contentWidth
		if chunkLen > len(rawRemaining) {
			chunkLen = len(rawRemaining)
		}
		if chunkLen == 0 {
			chunkLen = 1
		}

		// Find a good break point (prefer space)
		if chunkLen < len(rawRemaining) {
			breakAt := chunkLen
			for i := chunkLen - 1; i > chunkLen/2; i-- {
				if i < len(rawRemaining) && rawRemaining[i] == ' ' {
					breakAt = i + 1
					break
				}
			}
			chunkLen = breakAt
		}

		chunk, rest := splitAtVisibleLength(remaining, chunkLen)

		// Prefix chunk with any active colors from previous chunk
		chunkWithColors := activeColors + chunk

		if isFirst {
			lineNumStr := fmt.Sprintf("%*d", lineNumWidth, lineNum)
			b.WriteString(buildLine(lineNumStr, chunkWithColors))
			isFirst = false
		} else {
			// Continuation line: spaces instead of line number
			b.WriteString(buildLine(strings.Repeat(" ", lineNumWidth), chunkWithColors))
		}

		// Track active colors at end of this chunk for next iteration
		activeColors = getActiveAnsiCodes(chunkWithColors)

		remaining = rest
	}

	return b.String()
}

// diffAnsiRegex matches ANSI escape sequences for length calculation
var diffAnsiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// getActiveAnsiCodes returns the ANSI codes that are "active" at the end of a string.
// It tracks foreground colors set by \x1b[3Xm or \x1b[38;...m and returns them,
// accounting for resets (\x1b[0m or \x1b[39m).
func getActiveAnsiCodes(s string) string {
	var activeFg string

	// Find all ANSI sequences and track state
	matches := diffAnsiRegex.FindAllStringIndex(s, -1)
	for _, match := range matches {
		code := s[match[0]:match[1]]

		// Check for resets
		if code == "\x1b[0m" || code == "\x1b[39m" {
			activeFg = ""
			continue
		}

		// Check for foreground color codes (30-37, 38;..., 90-97)
		if strings.HasPrefix(code, "\x1b[3") || strings.HasPrefix(code, "\x1b[9") {
			activeFg = code
		}
	}

	return activeFg
}

// stripAnsi removes ANSI escape codes from a string for length calculation
func stripAnsi(s string) string {
	return diffAnsiRegex.ReplaceAllString(s, "")
}

// splitAtVisibleLength splits a string with ANSI codes at a visible character position
func splitAtVisibleLength(s string, visibleLen int) (string, string) {
	var result strings.Builder
	visible := 0
	i := 0

	for i < len(s) && visible < visibleLen {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			// ANSI escape sequence - copy until 'm'
			start := i
			for i < len(s) && s[i] != 'm' {
				i++
			}
			if i < len(s) {
				i++ // include the 'm'
			}
			result.WriteString(s[start:i])
		} else {
			result.WriteByte(s[i])
			visible++
			i++
		}
	}

	return result.String(), s[i:]
}

// diffSegment represents a segment of text with a change marker
type diffSegment struct {
	text      string
	isChanged bool
}

// wordDiff compares two lines word-by-word and returns segments marking changed portions.
// Uses LCS of words to identify common portions.
func wordDiff(oldLine, newLine string) (oldSegs, newSegs []diffSegment) {
	oldWords := splitIntoTokens(oldLine)
	newWords := splitIntoTokens(newLine)

	// Find LCS of words
	lcs := computeWordLCS(oldWords, newWords)

	// Build segments for old line
	oldSegs = buildSegments(oldWords, lcs)
	// Build segments for new line
	newSegs = buildSegments(newWords, lcs)

	return oldSegs, newSegs
}

// splitIntoTokens splits a line into tokens, preserving whitespace as separate tokens.
// This allows us to track whitespace changes precisely.
func splitIntoTokens(s string) []string {
	var tokens []string
	var current strings.Builder
	inSpace := false

	for _, r := range s {
		isSpace := r == ' ' || r == '\t'
		if isSpace != inSpace {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
			inSpace = isSpace
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}

// computeWordLCS computes the longest common subsequence of word tokens
func computeWordLCS(old, new []string) []string {
	m, n := len(old), len(new)
	if m == 0 || n == 0 {
		return nil
	}

	// Build LCS length table
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if old[i-1] == new[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack to find LCS
	lcsLen := dp[m][n]
	if lcsLen == 0 {
		return nil
	}

	lcs := make([]string, lcsLen)
	i, j := m, n
	for i > 0 && j > 0 {
		if old[i-1] == new[j-1] {
			lcsLen--
			lcs[lcsLen] = old[i-1]
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			i--
		} else {
			j--
		}
	}

	return lcs
}

// buildSegments converts a list of words into segments by comparing against LCS
func buildSegments(words []string, lcs []string) []diffSegment {
	if len(words) == 0 {
		return nil
	}

	var segments []diffSegment
	wordIdx := 0
	lcsIdx := 0

	for wordIdx < len(words) {
		// Check if current word matches next LCS word
		if lcsIdx < len(lcs) && words[wordIdx] == lcs[lcsIdx] {
			// This word is in LCS - unchanged
			segments = append(segments, diffSegment{
				text:      words[wordIdx],
				isChanged: false,
			})
			lcsIdx++
			wordIdx++
		} else {
			// This word is not in LCS - changed
			segments = append(segments, diffSegment{
				text:      words[wordIdx],
				isChanged: true,
			})
			wordIdx++
		}
	}

	return segments
}

// shouldUseWordDiff determines if word-level diff should be used for a line pair.
// Returns true if lines are similar enough (>50% words in common).
func shouldUseWordDiff(oldLine, newLine string) bool {
	oldWords := splitIntoTokens(oldLine)
	newWords := splitIntoTokens(newLine)

	if len(oldWords) == 0 || len(newWords) == 0 {
		return false
	}

	// Count non-whitespace tokens
	oldNonSpace := 0
	for _, w := range oldWords {
		if strings.TrimSpace(w) != "" {
			oldNonSpace++
		}
	}
	newNonSpace := 0
	for _, w := range newWords {
		if strings.TrimSpace(w) != "" {
			newNonSpace++
		}
	}

	if oldNonSpace == 0 || newNonSpace == 0 {
		return false
	}

	// Compute LCS and check similarity
	lcs := computeWordLCS(oldWords, newWords)

	// Count non-whitespace tokens in LCS
	lcsNonSpace := 0
	for _, w := range lcs {
		if strings.TrimSpace(w) != "" {
			lcsNonSpace++
		}
	}

	// Calculate similarity as ratio of common words to smaller line
	minWords := oldNonSpace
	if newNonSpace < minWords {
		minWords = newNonSpace
	}

	// Use word diff if >50% words are in common
	return float64(lcsNonSpace)/float64(minWords) > 0.5
}

// applyWordDiffHighlight takes segments and applies appropriate background colors.
// Changed segments get strongBg, unchanged segments get normalBg.
// highlighter is used for syntax highlighting within segments.
func applyWordDiffHighlight(segments []diffSegment, highlighter *Highlighter, normalBg, strongBg [3]int) string {
	var b strings.Builder

	for _, seg := range segments {
		bg := normalBg
		if seg.isChanged {
			bg = strongBg
		}

		// Apply background and highlight segment
		if highlighter != nil {
			b.WriteString(highlighter.HighlightLineWithBg(seg.text, bg))
		} else {
			b.WriteString(fmt.Sprintf("\x1b[48;2;%d;%d;%dm%s\x1b[0m", bg[0], bg[1], bg[2], seg.text))
		}
	}

	return b.String()
}

// wrapWordDiffLine wraps a line that already has word-diff highlighting applied.
// Unlike wrapDiffLine, this doesn't apply a uniform background - the content already has mixed backgrounds.
// paddingBg is used for trailing padding on each line.
func wrapWordDiffLine(lineNumWidth int, lineNum int, prefix byte, prefixColor string, content string, contentWidth int, paddingBg [3]int) string {
	bgCode := fmt.Sprintf("\x1b[48;2;%d;%d;%dm", paddingBg[0], paddingBg[1], paddingBg[2])

	// Helper to build a complete line
	buildLine := func(lineNumStr string, chunk string) string {
		visibleLen := len(stripAnsi(chunk))
		padding := ""
		if visibleLen < contentWidth {
			padding = strings.Repeat(" ", contentWidth-visibleLen)
		}
		// Format: bg + prefix_color + line_num + prefix + reset + content (with its own backgrounds) + padding_bg + padding + reset
		return fmt.Sprintf("%s%s%s%c \x1b[0m%s%s%s\x1b[0m\n", bgCode, prefixColor, lineNumStr, prefix, chunk, bgCode, padding)
	}

	// If content fits, return single padded line
	rawLen := len(stripAnsi(content))
	if rawLen <= contentWidth {
		lineNumStr := fmt.Sprintf("%*d", lineNumWidth, lineNum)
		return buildLine(lineNumStr, content)
	}

	// Need to wrap - split content into chunks
	var b strings.Builder
	remaining := content
	isFirst := true
	activeColors := "" // Track active ANSI codes (fg and bg) across chunks

	for len(remaining) > 0 {
		rawRemaining := stripAnsi(remaining)
		if len(rawRemaining) == 0 {
			break
		}

		chunkLen := contentWidth
		if chunkLen > len(rawRemaining) {
			chunkLen = len(rawRemaining)
		}
		if chunkLen == 0 {
			chunkLen = 1
		}

		// Find a good break point (prefer space)
		if chunkLen < len(rawRemaining) {
			breakAt := chunkLen
			for i := chunkLen - 1; i > chunkLen/2; i-- {
				if i < len(rawRemaining) && rawRemaining[i] == ' ' {
					breakAt = i + 1
					break
				}
			}
			chunkLen = breakAt
		}

		chunk, rest := splitAtVisibleLength(remaining, chunkLen)

		// Prefix chunk with any active colors from previous chunk
		chunkWithColors := activeColors + chunk

		if isFirst {
			lineNumStr := fmt.Sprintf("%*d", lineNumWidth, lineNum)
			b.WriteString(buildLine(lineNumStr, chunkWithColors))
			isFirst = false
		} else {
			// Continuation line: spaces instead of line number
			b.WriteString(buildLine(strings.Repeat(" ", lineNumWidth), chunkWithColors))
		}

		// Track active colors at end of this chunk for next iteration
		activeColors = getActiveAnsiCodesWithBg(chunkWithColors)

		remaining = rest
	}

	return b.String()
}

// getActiveAnsiCodesWithBg returns active ANSI codes at end of string, including both fg and bg.
func getActiveAnsiCodesWithBg(s string) string {
	var activeFg, activeBg string

	// Find all ANSI sequences and track state
	matches := diffAnsiRegex.FindAllStringIndex(s, -1)
	for _, match := range matches {
		code := s[match[0]:match[1]]

		// Check for resets
		if code == "\x1b[0m" {
			activeFg = ""
			activeBg = ""
			continue
		}

		// Check for foreground reset
		if code == "\x1b[39m" {
			activeFg = ""
			continue
		}

		// Check for background reset
		if code == "\x1b[49m" {
			activeBg = ""
			continue
		}

		// Check for foreground color codes (30-37, 38;..., 90-97)
		if strings.HasPrefix(code, "\x1b[3") && !strings.HasPrefix(code, "\x1b[38;2") {
			activeFg = code
		} else if strings.HasPrefix(code, "\x1b[38;") {
			activeFg = code
		} else if strings.HasPrefix(code, "\x1b[9") && len(code) > 4 {
			// 90-97 are bright foreground colors
			activeFg = code
		}

		// Check for background color codes (40-47, 48;..., 100-107)
		if strings.HasPrefix(code, "\x1b[4") && !strings.HasPrefix(code, "\x1b[48;2") {
			// Basic background 40-47
			activeBg = code
		} else if strings.HasPrefix(code, "\x1b[48;") {
			// True color background
			activeBg = code
		} else if strings.HasPrefix(code, "\x1b[10") {
			// Bright background 100-107
			activeBg = code
		}
	}

	return activeBg + activeFg
}

// bufferedRemoval holds a buffered removal line for pairing with additions
type bufferedRemoval struct {
	content string
	lineNum int
}

// RenderDiffSegment renders a unified diff as a string for inline display.
// Returns empty string if no changes or on error.
// The width parameter is used for wrapping long lines.
// The startLine parameter is the 1-indexed line number where the edit starts (0 = use diff header).
func RenderDiffSegment(filePath, oldContent, newContent string, width int, startLine int) string {
	if oldContent == newContent {
		return ""
	}

	styles := DefaultStyles()
	var b strings.Builder

	// Print header
	b.WriteString(fmt.Sprintf("%s %s\n", styles.Bold.Render("Edit:"), filePath))

	// Generate unified diff using gotextdiff
	diffBytes := diff.Diff(filePath, []byte(oldContent), filePath, []byte(newContent))
	if len(diffBytes) == 0 {
		return ""
	}

	diffText := string(diffBytes)

	// Create highlighter for syntax highlighting
	highlighter := NewHighlighter(filePath)

	// Calculate line number width - use startLine + content lines if provided
	oldLines := strings.Count(oldContent, "\n") + 1
	newLines := strings.Count(newContent, "\n") + 1
	maxLine := oldLines
	if newLines > maxLine {
		maxLine = newLines
	}
	if startLine > 0 {
		maxLine += startLine - 1
	}
	lineNumWidth := len(strconv.Itoa(maxLine))
	if lineNumWidth < 3 {
		lineNumWidth = 3
	}

	// Calculate content width based on terminal width parameter
	// Subtract line number width + 2 (for prefix char and space)
	prefixWidth := lineNumWidth + 2
	contentWidth := maxDiffContentWidth
	if width > 0 && width-prefixWidth < contentWidth {
		contentWidth = width - prefixWidth
		if contentWidth < 40 {
			contentWidth = 40 // minimum content width
		}
	}

	// Track current line numbers
	var oldLineNum, newLineNum int
	hunkCount := 0
	renderedLines := 0
	truncated := false

	// Buffer for removal lines (to pair with additions for word-diff)
	var removalBuffer []bufferedRemoval

	// Regex to parse hunk header: @@ -start,count +start,count @@
	hunkRe := regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

	// Helper to render a removal line without word-diff
	renderRemoval := func(content string, lineNum int) {
		highlighted := content
		if highlighter != nil {
			highlighted = highlighter.HighlightLine(content)
		}
		b.WriteString(wrapDiffLine(lineNumWidth, lineNum, '-', "\x1b[38;2;160;80;80m", highlighted, contentWidth, diffRemoveBg[:]))
		renderedLines++
	}

	// Helper to render an addition line without word-diff
	renderAddition := func(content string, lineNum int) {
		highlighted := content
		if highlighter != nil {
			highlighted = highlighter.HighlightLine(content)
		}
		b.WriteString(wrapDiffLine(lineNumWidth, lineNum, '+', "\x1b[38;2;80;160;80m", highlighted, contentWidth, diffAddBg[:]))
		renderedLines++
	}

	// Helper to render a removal line with word-diff highlighting
	renderRemovalWithWordDiff := func(_ string, lineNum int, segments []diffSegment) {
		highlighted := applyWordDiffHighlight(segments, highlighter, diffRemoveBg, diffRemoveBgStrong)
		b.WriteString(wrapWordDiffLine(lineNumWidth, lineNum, '-', "\x1b[38;2;160;80;80m", highlighted, contentWidth, diffRemoveBg))
		renderedLines++
	}

	// Helper to render an addition line with word-diff highlighting
	renderAdditionWithWordDiff := func(_ string, lineNum int, segments []diffSegment) {
		highlighted := applyWordDiffHighlight(segments, highlighter, diffAddBg, diffAddBgStrong)
		b.WriteString(wrapWordDiffLine(lineNumWidth, lineNum, '+', "\x1b[38;2;80;160;80m", highlighted, contentWidth, diffAddBg))
		renderedLines++
	}

	// Helper to flush buffered removals (renders them without word-diff)
	// Respects maxDiffLines limit
	flushRemovals := func() {
		for _, rem := range removalBuffer {
			if renderedLines >= maxDiffLines {
				truncated = true
				break
			}
			renderRemoval(rem.content, rem.lineNum)
		}
		removalBuffer = nil
	}

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

		// Check if we've hit the line limit
		if renderedLines >= maxDiffLines {
			truncated = true
			break
		}

		prefix := line[0]
		content := ""
		if len(line) > 1 {
			content = line[1:]
		}

		switch prefix {
		case '@':
			// Flush any buffered removals before new hunk
			flushRemovals()

			// Parse hunk header to get starting line numbers
			if matches := hunkRe.FindStringSubmatch(line); matches != nil {
				oldLineNum, _ = strconv.Atoi(matches[1])
				newLineNum, _ = strconv.Atoi(matches[2])
				// Apply offset if startLine is provided
				if startLine > 0 {
					offset := startLine - 1
					oldLineNum += offset
					newLineNum += offset
				}
			}
			// Show "..." separator between hunks (not before first one)
			if hunkCount > 0 {
				b.WriteString(fmt.Sprintf("\x1b[38;2;100;100;100m%s\x1b[0m\n", strings.Repeat(" ", lineNumWidth)+"  ..."))
				renderedLines++
			}
			hunkCount++

		case '-':
			// Buffer removal lines for potential pairing with additions
			removalBuffer = append(removalBuffer, bufferedRemoval{
				content: content,
				lineNum: oldLineNum,
			})
			oldLineNum++

		case '+':
			if len(removalBuffer) > 0 {
				// Pop the first buffered removal
				rem := removalBuffer[0]
				removalBuffer = removalBuffer[1:]

				// Check if word-diff should be used
				if shouldUseWordDiff(rem.content, content) {
					oldSegs, newSegs := wordDiff(rem.content, content)
					renderRemovalWithWordDiff(rem.content, rem.lineNum, oldSegs)
					renderAdditionWithWordDiff(content, newLineNum, newSegs)
				} else {
					// Lines are too different, render without word-diff
					renderRemoval(rem.content, rem.lineNum)
					renderAddition(content, newLineNum)
				}
			} else {
				// Pure addition (no removal to pair with)
				renderAddition(content, newLineNum)
			}
			newLineNum++

		case ' ':
			// Flush any buffered removals before context line
			flushRemovals()

			// Context line - no background, show line number in grey
			highlighted := content
			if highlighter != nil {
				highlighted = highlighter.HighlightLine(content)
			}
			b.WriteString(wrapDiffLine(lineNumWidth, newLineNum, ' ', "\x1b[38;2;100;100;100m", highlighted, contentWidth, nil))
			oldLineNum++
			newLineNum++
			renderedLines++

		case '\\':
			// Skip diff metadata lines like "\ No newline at end of file"
			continue

		default:
			// Unknown prefix - skip (shouldn't happen normally)
			continue
		}
	}

	// Flush any remaining buffered removals
	flushRemovals()

	// Show truncation notice if we hit the limit
	if truncated {
		remaining := 0
		for _, line := range lines {
			if len(line) > 0 && (line[0] == '+' || line[0] == '-' || line[0] == ' ') {
				remaining++
			}
		}
		remaining -= renderedLines
		if remaining > 0 {
			b.WriteString(fmt.Sprintf("\x1b[38;2;100;100;100m[... %d more lines ...]\x1b[0m\n", remaining))
		}
	}

	return b.String()
}
