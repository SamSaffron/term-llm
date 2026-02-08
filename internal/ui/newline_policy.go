package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	// SectionBreakTrailingNewlines is the minimum trailing newline run that
	// separates adjacent output sections.
	SectionBreakTrailingNewlines = 1
	// FinalSpacerTrailingNewlines is the minimum trailing newline run to keep
	// one spacer line before the next prompt after completion.
	FinalSpacerTrailingNewlines = 2
)

// CountTrailingNewlines returns how many '\n' characters appear at the end of s.
func CountTrailingNewlines(s string) int {
	count := 0
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '\n' {
			break
		}
		count++
	}
	return count
}

// NewlinesNeededForTrailing returns the number of '\n' characters required to
// reach at least targetTrailing newlines.
func NewlinesNeededForTrailing(currentTrailing, targetTrailing int) int {
	if currentTrailing >= targetTrailing {
		return 0
	}
	return targetTrailing - currentTrailing
}

// NewlinePadding returns the minimal newline padding needed to reach at least
// targetTrailing trailing newlines.
func NewlinePadding(currentTrailing, targetTrailing int) string {
	return strings.Repeat("\n", NewlinesNeededForTrailing(currentTrailing, targetTrailing))
}

// ScrollbackPrintlnCommands returns tea.Println command(s) for content and an
// optional final spacer line. It preserves content while avoiding synthetic
// double-newline inflation from unconditional blank-line commands.
func ScrollbackPrintlnCommands(content string, includeFinalSpacer bool) []tea.Cmd {
	if content == "" {
		if includeFinalSpacer {
			return []tea.Cmd{tea.Println("")}
		}
		return nil
	}

	cmds := []tea.Cmd{tea.Println(content)}
	if includeFinalSpacer {
		// tea.Println always appends one newline of its own.
		postPrintTrailing := CountTrailingNewlines(content) + 1
		if postPrintTrailing < FinalSpacerTrailingNewlines {
			cmds = append(cmds, tea.Println(""))
		}
	}
	return cmds
}
