package ui

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	diffPrefixWidth = 6 // "1234- " or "1234+ " or "1234  "
)

// getMaxContentWidth returns the max content width for diff lines based on terminal width
// Prefers 100, but falls back to 80 or 60 for narrow terminals
func getMaxContentWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		width = 80 // fallback
	}

	// Available width for content after prefix
	available := width - diffPrefixWidth

	// Prefer 100, but use 80 or 60 for narrow terminals
	switch {
	case available >= 100:
		return 100
	case available >= 80:
		return 80
	case available >= 60:
		return 60
	default:
		return available
	}
}

// wrapLine wraps a line to maxWidth, returning multiple lines
// Continuation lines are indented with 2 spaces
func wrapLine(line string, maxWidth int) []string {
	if maxWidth <= 0 || len(line) <= maxWidth {
		return []string{line}
	}

	var result []string
	remaining := line
	first := true

	for len(remaining) > 0 {
		width := maxWidth
		if !first {
			width = maxWidth - 2 // account for continuation indent
		}
		if width <= 0 {
			width = 10 // minimum
		}

		if len(remaining) <= width {
			if first {
				result = append(result, remaining)
			} else {
				result = append(result, "  "+remaining)
			}
			break
		}

		// Find a good break point (prefer space/punctuation)
		breakAt := width
		for i := width - 1; i > width/2; i-- {
			if remaining[i] == ' ' || remaining[i] == ',' || remaining[i] == ';' ||
				remaining[i] == '.' || remaining[i] == ')' || remaining[i] == '}' {
				breakAt = i + 1
				break
			}
		}

		if first {
			result = append(result, remaining[:breakAt])
		} else {
			result = append(result, "  "+remaining[:breakAt])
		}
		remaining = remaining[breakAt:]
		first = false
	}

	return result
}

// PrintCompactDiff prints a compact diff with 2 lines of context and line numbers
// padWidth specifies the total line width for consistent backgrounds across diffs
func PrintCompactDiff(filePath, oldContent, newContent string, padWidth int) {
	styles := DefaultStyles()

	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	// Find all changed regions
	changes := computeChanges(oldLines, newLines)

	if len(changes) == 0 {
		return
	}

	// Print header
	fmt.Printf("%s %s\n", styles.Bold.Render("Edit:"), filePath)

	const contextLines = 2

	// Get max content width based on terminal
	maxContentWidth := getMaxContentWidth()

	// Cap padWidth to maxContentWidth + prefix
	maxPadWidth := maxContentWidth + diffPrefixWidth
	if padWidth > maxPadWidth {
		padWidth = maxPadWidth
	}

	padLine := func(s string) string {
		if len(s) < padWidth {
			return s + strings.Repeat(" ", padWidth-len(s)) + " "
		}
		return s + " "
	}

	// printWrapped prints a line with wrapping, applying style to each wrapped segment
	printWrapped := func(lineNum int, marker string, content string, style func(...string) string) {
		wrapped := wrapLine(content, maxContentWidth)
		for i, segment := range wrapped {
			var formatted string
			if i == 0 {
				formatted = fmt.Sprintf("%4d%s %s", lineNum, marker, segment)
			} else {
				// Continuation: no line number, keep marker column aligned
				formatted = fmt.Sprintf("    %s %s", marker, segment)
			}
			fmt.Println(style(padLine(formatted)))
		}
	}

	printElision := func() {
		fmt.Println(styles.Muted.Render(padLine("   ...")))
	}

	lastPrintedOld := -1 // track last old line we printed

	for i, ch := range changes {
		// Calculate context range for this hunk
		ctxStart := ch.oldStart - contextLines
		if ctxStart < 0 {
			ctxStart = 0
		}
		ctxEnd := ch.oldStart + ch.oldCount + contextLines
		if ctxEnd > len(oldLines) {
			ctxEnd = len(oldLines)
		}

		// Show elision if there's a gap from last printed content
		if i > 0 && ctxStart > lastPrintedOld+1 {
			printElision()
		}

		// Print context before (skip if already printed by previous hunk)
		for j := ctxStart; j < ch.oldStart; j++ {
			if j > lastPrintedOld && j < len(oldLines) {
				printWrapped(j+1, " ", oldLines[j], styles.DiffContext.Render)
				lastPrintedOld = j
			}
		}

		// Print removed lines
		for j := ch.oldStart; j < ch.oldStart+ch.oldCount; j++ {
			if j < len(oldLines) {
				printWrapped(j+1, "-", oldLines[j], styles.DiffRemove.Render)
				lastPrintedOld = j
			}
		}

		// Print added lines
		for j := ch.newStart; j < ch.newStart+ch.newCount; j++ {
			if j < len(newLines) {
				printWrapped(j+1, "+", newLines[j], styles.DiffAdd.Render)
			}
		}

		// Print context after
		for j := ch.oldStart + ch.oldCount; j < ctxEnd; j++ {
			if j > lastPrintedOld && j < len(oldLines) {
				printWrapped(j+1, " ", oldLines[j], styles.DiffContext.Render)
				lastPrintedOld = j
			}
		}
	}
}

// CalcDiffWidth calculates the required padding width for a diff
// The result is capped to the terminal-aware max content width
func CalcDiffWidth(oldContent, newContent string) int {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	changes := computeChanges(oldLines, newLines)

	const contextLines = 2
	maxLen := 0

	for _, ch := range changes {
		start := ch.oldStart - contextLines
		if start < 0 {
			start = 0
		}
		end := ch.oldStart + ch.oldCount + contextLines
		if end > len(oldLines) {
			end = len(oldLines)
		}
		for i := start; i < end; i++ {
			if len(oldLines[i]) > maxLen {
				maxLen = len(oldLines[i])
			}
		}
		for i := ch.newStart; i < ch.newStart+ch.newCount; i++ {
			if i < len(newLines) && len(newLines[i]) > maxLen {
				maxLen = len(newLines[i])
			}
		}
	}

	// Cap to terminal-aware max content width
	maxContentWidth := getMaxContentWidth()
	if maxLen > maxContentWidth {
		maxLen = maxContentWidth
	}

	// Add prefix width (line number + marker + space = 6 chars)
	return maxLen + diffPrefixWidth
}

type change struct {
	oldStart, oldCount int
	newStart, newCount int
}

// computeChanges finds individual changed regions (hunks) between old and new
func computeChanges(old, new []string) []change {
	if len(old) == 0 && len(new) == 0 {
		return nil
	}

	// Use LCS-based diff to find matching lines
	lcs := computeLCS(old, new)

	var changes []change
	oldIdx, newIdx := 0, 0
	lcsIdx := 0

	for oldIdx < len(old) || newIdx < len(new) {
		// Skip matching lines
		for lcsIdx < len(lcs) && oldIdx < len(old) && newIdx < len(new) &&
			old[oldIdx] == lcs[lcsIdx] && new[newIdx] == lcs[lcsIdx] {
			oldIdx++
			newIdx++
			lcsIdx++
		}

		// Find the extent of this change
		oldStart := oldIdx
		newStart := newIdx

		// Advance old until we hit the next LCS line
		for oldIdx < len(old) && (lcsIdx >= len(lcs) || old[oldIdx] != lcs[lcsIdx]) {
			oldIdx++
		}

		// Advance new until we hit the next LCS line
		for newIdx < len(new) && (lcsIdx >= len(lcs) || new[newIdx] != lcs[lcsIdx]) {
			newIdx++
		}

		oldCount := oldIdx - oldStart
		newCount := newIdx - newStart

		if oldCount > 0 || newCount > 0 {
			changes = append(changes, change{
				oldStart: oldStart,
				oldCount: oldCount,
				newStart: newStart,
				newCount: newCount,
			})
		}
	}

	return changes
}

// computeLCS computes the longest common subsequence of lines
func computeLCS(old, new []string) []string {
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

// ShowEditSkipped shows that an edit was skipped
func ShowEditSkipped(filePath string, reason string) {
	styles := DefaultStyles()
	fmt.Printf("%s %s: %s\n", styles.Muted.Render("○"), filePath, reason)
}

// PromptApplyEdit asks the user whether to apply an edit
// Returns true if user wants to apply (Enter or y), false to skip (n)
func PromptApplyEdit() bool {
	styles := DefaultStyles()

	// Show prompt: "Apply? (Y/n) "
	fmt.Print("Apply? " + styles.Muted.Render("(Y/n)") + " ")

	// Set terminal to raw mode to read single keypress
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println()
		return true // default yes on error
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	b := make([]byte, 1)
	os.Stdin.Read(b)

	// Enter or y/Y means yes, n/N means no
	applied := b[0] == 'y' || b[0] == 'Y' || b[0] == '\r' || b[0] == '\n'

	// Show response in white
	if applied {
		fmt.Println("Y")
	} else {
		fmt.Println("n")
	}

	return applied
}
