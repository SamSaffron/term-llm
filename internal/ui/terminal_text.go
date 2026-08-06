package ui

import (
	"strings"
	"unicode"

	xansi "github.com/charmbracelet/x/ansi"
)

// sanitizeTerminalText removes terminal control sequences from text that came
// from a provider, tool arguments, or tool output before it enters the TUI
// renderer. The renderer deliberately understands ANSI in its own generated
// output; allowing untrusted display metadata to carry ANSI would let it erase,
// move, or restyle unrelated cells in the frame.
func sanitizeTerminalText(s string) string {
	if s == "" {
		return ""
	}

	// x/ansi handles the common 7-bit ESC-prefixed forms. Expand C1 control
	// introducers first so their 8-bit equivalents are stripped as well.
	if strings.ContainsAny(s, "\u0090\u0098\u009b\u009d\u009e\u009f") {
		var expanded strings.Builder
		expanded.Grow(len(s))
		for _, r := range s {
			switch r {
			case '\u0090':
				expanded.WriteString("\x1bP")
			case '\u0098':
				expanded.WriteString("\x1bX")
			case '\u009b':
				expanded.WriteString("\x1b[")
			case '\u009d':
				expanded.WriteString("\x1b]")
			case '\u009e':
				expanded.WriteString("\x1b^")
			case '\u009f':
				expanded.WriteString("\x1b_")
			default:
				expanded.WriteRune(r)
			}
		}
		s = expanded.String()
	}
	s = xansi.Strip(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")

	var clean strings.Builder
	clean.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\n', '\t':
			clean.WriteRune(r)
		case '\r':
			// A lone carriage return is meaningful terminal cursor movement.
			// Preserve its textual line-break intent without executing it.
			clean.WriteByte('\n')
		default:
			if !unicode.IsControl(r) {
				clean.WriteRune(r)
			}
		}
	}
	return clean.String()
}
