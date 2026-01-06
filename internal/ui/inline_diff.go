package ui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"golang.org/x/term"
)

// getMaxContentWidth returns the max content width for diff lines based on terminal width
// Prefers 100, but falls back to 80 or 60 for narrow terminals
func getMaxContentWidth(prefixWidth int) int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		width = 80 // fallback
	}

	// Available width for content after prefix
	available := width - prefixWidth

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

func diffPrefixWidths(oldLines, newLines []string) (lineNumWidth int, prefixWidth int) {
	maxLine := len(oldLines)
	if len(newLines) > maxLine {
		maxLine = len(newLines)
	}
	if maxLine < 1 {
		maxLine = 1
	}
	lineNumWidth = len(strconv.Itoa(maxLine))
	if lineNumWidth < 1 {
		lineNumWidth = 1
	}
	prefixWidth = lineNumWidth + 2 // marker + trailing space
	return lineNumWidth, prefixWidth
}

// wrapLine wraps a line to maxWidth, returning multiple lines.
// Continuation lines are indented with 2 spaces.
// startCol is the column where the line begins (used for tab alignment).
// This version is ANSI-aware and handles escape codes properly.
func wrapLine(line string, maxWidth int, startCol int) []string {
	displayLen := ANSILen(line)
	if maxWidth <= 0 || displayLen <= maxWidth {
		return []string{line}
	}

	var result []string
	remaining := line
	first := true

	for ANSILen(remaining) > 0 {
		width := maxWidth
		if !first {
			width = maxWidth - 2 // account for continuation indent
		}
		if width <= 0 {
			width = 10 // minimum
		}

		if ansiDisplayWidth(remaining, startCol) <= width {
			if first {
				result = append(result, remaining)
			} else {
				result = append(result, "  "+remaining)
			}
			break
		}

		// Split at display width, preferring word boundaries
		segment, rest := splitAtDisplayWidthPreferBreak(remaining, width, startCol)

		if first {
			result = append(result, segment)
		} else {
			result = append(result, "  "+segment)
		}
		remaining = rest
		first = false

		if rest == "" {
			break
		}
	}

	return result
}

// splitAtDisplayWidthPreferBreak splits a string at approximately the given display width,
// preferring to break at word boundaries (space, punctuation)
// preserving ANSI sequences intact, using startCol for tab alignment.
func splitAtDisplayWidthPreferBreak(s string, width int, startCol int) (before, after string) {
	inEscape := false
	col := startCol
	lastBreakDisplay := -1
	lastBreakByte := -1

	for i := 0; i < len(s); {
		c := s[i]
		if c == '\x1b' {
			inEscape = true
			i++
			continue
		}
		if inEscape {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				inEscape = false
			}
			i++
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r = rune(c)
			size = 1
		}

		// Check if this is a break character
		isBreak := r == ' ' || r == ',' || r == ';' || r == '.' || r == ')' || r == '}'

		col = advanceColumn(col, r)
		displayPos := col - startCol
		byteEnd := i + size

		if isBreak && displayPos <= width {
			lastBreakDisplay = displayPos
			lastBreakByte = byteEnd
		}

		if displayPos >= width {
			// We've hit the width limit
			// Prefer breaking at last break point if it's in the second half
			if lastBreakByte > 0 && lastBreakDisplay > width/2 {
				return s[:lastBreakByte], s[lastBreakByte:]
			}
			// No good break point, hard break at width
			return s[:byteEnd], s[byteEnd:]
		}

		i += size
	}

	return s, ""
}

// Background colors for diff lines (RGB values for true color)
// These are very subtle tints - almost black with a slight color hint
var (
	diffAddBg    = [3]int{26, 47, 26} // dark green tint #1a2f1a
	diffRemoveBg = [3]int{47, 26, 26} // dark red tint #2f1a1a
	diffNoBg     = [3]int{-1, -1, -1} // sentinel for no background (context lines)
)

// PrintCompactDiff prints a compact diff with 2 lines of context and line numbers
// padWidth specifies the total line width for consistent backgrounds across diffs
func PrintCompactDiff(filePath, oldContent, newContent string, padWidth int) {
	styles := DefaultStyles()

	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	// Create highlighter based on file extension
	highlighter := NewHighlighter(filePath)

	// Find all changed regions
	changes := computeChanges(oldLines, newLines)

	if len(changes) == 0 {
		return
	}

	// Print header
	fmt.Printf("%s %s\n", styles.Bold.Render("Edit:"), filePath)

	const contextLines = 2

	lineNumWidth, prefixWidth := diffPrefixWidths(oldLines, newLines)

	// Get max content width based on terminal
	maxContentWidth := getMaxContentWidth(prefixWidth)

	// Cap padWidth to maxContentWidth + prefix
	maxPadWidth := maxContentWidth + prefixWidth
	if padWidth <= 0 || padWidth > maxPadWidth {
		padWidth = maxPadWidth
	}
	contentWidth := padWidth - prefixWidth
	if contentWidth < 1 {
		contentWidth = maxContentWidth
		padWidth = contentWidth + prefixWidth
	}

	// padLine pads a string to padWidth, accounting for ANSI codes
	padLine := func(s string) string {
		displayLen := ANSILen(s)
		if displayLen < padWidth {
			return s + strings.Repeat(" ", padWidth-displayLen) + " "
		}
		return s + " "
	}

	// hasBg checks if background should be applied
	hasBg := func(bg [3]int) bool {
		return bg[0] >= 0
	}

	// bgCode generates ANSI true color background escape sequence
	bgCode := func(bg [3]int) string {
		if !hasBg(bg) {
			return ""
		}
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", bg[0], bg[1], bg[2])
	}

	// highlightLine applies syntax highlighting with optional background
	highlightLine := func(line string, bg [3]int) string {
		if highlighter != nil {
			if hasBg(bg) {
				return highlighter.HighlightLineWithBg(line, bg)
			}
			return highlighter.HighlightLine(line)
		}
		// No syntax highlighting
		if hasBg(bg) {
			return fmt.Sprintf("%s%s\x1b[0m", bgCode(bg), line)
		}
		return line
	}

	// printWrapped prints a line with wrapping and syntax highlighting
	// We wrap first, then highlight each segment so ANSI codes don't get split
	printWrapped := func(lineNum int, marker, content string, bg [3]int) {
		wrapped := wrapLine(content, contentWidth, prefixWidth)
		// Color for line number and marker based on diff type
		var prefixColor string
		switch marker {
		case "+":
			prefixColor = "\x1b[38;2;80;160;80m" // green for additions
		case "-":
			prefixColor = "\x1b[38;2;160;80;80m" // red for removals
		default:
			prefixColor = "\x1b[38;2;100;100;100m" // grey for context
		}
		useBg := hasBg(bg)
		for i, segment := range wrapped {
			// Strip continuation indent for highlighting, then re-add it
			textToHighlight := segment
			continuationIndent := ""
			if i > 0 && strings.HasPrefix(segment, "  ") {
				textToHighlight = segment[2:]
				continuationIndent = "  "
			}
			highlighted := continuationIndent + highlightLine(textToHighlight, bg)

			var prefix string
			if i == 0 {
				prefix = fmt.Sprintf("%s%s%*d%s ", bgCode(bg), prefixColor, lineNumWidth, lineNum, marker)
			} else {
				prefix = fmt.Sprintf("%s%s%s%s ", bgCode(bg), prefixColor, strings.Repeat(" ", lineNumWidth), marker)
			}
			line := prefix + highlighted
			// Only pad with background for changed lines
			if useBg {
				displayLen := ANSILen(line)
				if displayLen < padWidth {
					line = line + bgCode(bg) + strings.Repeat(" ", padWidth-displayLen)
				}
			}
			fmt.Println(line + "\x1b[0m")
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
				printWrapped(j+1, " ", oldLines[j], diffNoBg)
				lastPrintedOld = j
			}
		}

		// Print removed lines
		for j := ch.oldStart; j < ch.oldStart+ch.oldCount; j++ {
			if j < len(oldLines) {
				printWrapped(j+1, "-", oldLines[j], diffRemoveBg)
				lastPrintedOld = j
			}
		}

		// Print added lines
		for j := ch.newStart; j < ch.newStart+ch.newCount; j++ {
			if j < len(newLines) {
				printWrapped(j+1, "+", newLines[j], diffAddBg)
			}
		}

		// Print context after
		for j := ch.oldStart + ch.oldCount; j < ctxEnd; j++ {
			if j > lastPrintedOld && j < len(oldLines) {
				printWrapped(j+1, " ", oldLines[j], diffNoBg)
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
	_, prefixWidth := diffPrefixWidths(oldLines, newLines)

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
			lineWidth := ansiDisplayWidth(oldLines[i], prefixWidth)
			if lineWidth > maxLen {
				maxLen = lineWidth
			}
		}
		for i := ch.newStart; i < ch.newStart+ch.newCount; i++ {
			if i < len(newLines) {
				lineWidth := ansiDisplayWidth(newLines[i], prefixWidth)
				if lineWidth > maxLen {
					maxLen = lineWidth
				}
			}
		}
	}

	// Cap to terminal-aware max content width
	maxContentWidth := getMaxContentWidth(prefixWidth)
	if maxLen > maxContentWidth {
		maxLen = maxContentWidth
	}

	// Add prefix width (line number + marker + space)
	return maxLen + prefixWidth
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

// EditApprovalResult represents the result of batch approval prompt
type EditApprovalResult int

const (
	EditApprovalYes  EditApprovalResult = iota // Apply all changes
	EditApprovalNo                             // Skip all changes
	EditApprovalInfo                           // Show info/about text
)

// PromptBatchApproval asks user to approve all changes with option to see info
// Returns EditApprovalYes, EditApprovalNo, or EditApprovalInfo
// If reprompt is true, clears the line before showing prompt (used after returning from info)
func PromptBatchApproval(hasInfo bool, reprompt bool) EditApprovalResult {
	styles := DefaultStyles()

	// Clear line if reprompting after info
	if reprompt {
		fmt.Print("\r\033[K")
	}

	// Show prompt with or without info option
	if hasInfo {
		fmt.Print("Apply? " + styles.Muted.Render("(Y/n/i)") + " ")
	} else {
		fmt.Print("Apply? " + styles.Muted.Render("(Y/n)") + " ")
	}

	// Set terminal to raw mode to read single keypress
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println()
		return EditApprovalYes // default yes on error
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	b := make([]byte, 1)
	os.Stdin.Read(b)

	switch b[0] {
	case 'i', 'I':
		if hasInfo {
			// Don't print anything - we'll reprompt after info view
			return EditApprovalInfo
		}
		// No info available, treat as yes
		fmt.Println("Y")
		return EditApprovalYes
	case 'n', 'N':
		fmt.Println("n")
		return EditApprovalNo
	default:
		// Enter, y, Y, or anything else = yes
		fmt.Println("Y")
		return EditApprovalYes
	}
}

// ShowEditInfo displays the about/info text in a fullscreen pager
func ShowEditInfo(aboutText string) {
	width, height := getTerminalSize()

	// Create the model
	m := newInfoModel(aboutText, width, height)

	// Get TTY for terminal control
	tty, err := getTTY()
	if err != nil {
		// Fallback to simple display
		fmt.Println()
		fmt.Println(aboutText)
		fmt.Println()
		return
	}
	defer tty.Close()

	// Create program with alternate screen
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithInput(tty),
		tea.WithOutput(tty),
		tea.WithoutSignalHandler(),
	)

	p.Run()
}

// infoModel is the bubbletea model for fullscreen info display
type infoModel struct {
	viewport viewport.Model
	styles   *Styles
	width    int
	height   int
}

func newInfoModel(content string, width, height int) infoModel {
	styles := DefaultStyles()

	vp := viewport.New(width, height-1) // -1 for footer

	// Render markdown content
	rendered := renderInfoMarkdown(content, width)
	vp.SetContent(rendered)

	return infoModel{
		viewport: vp,
		styles:   styles,
		width:    width,
		height:   height,
	}
}

func (m infoModel) Init() tea.Cmd {
	return nil
}

func (m infoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "Q", "esc", "enter", "i", "I":
			return m, tea.Quit
		case "ctrl+c":
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - 1
	}

	// Update viewport for scrolling
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)

	return m, cmd
}

func (m infoModel) View() string {
	footer := m.styles.Footer.Render("↑/↓ scroll • q/Esc/Enter to exit")
	return m.viewport.View() + "\n" + footer
}

// renderInfoMarkdown renders content with glamour for info display
func renderInfoMarkdown(content string, width int) string {
	style := styles.DraculaStyleConfig
	style.Document.Margin = uintPtr(0)
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = uintPtr(0)

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width-2), // slight margin
	)
	if err != nil {
		return content
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}

	result := strings.TrimSpace(rendered)
	if result != "" && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	return result
}

