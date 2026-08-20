package terminaltext

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
)

// Sanitize removes terminal control sequences from untrusted display text while
// preserving its printable text, tabs, and line structure. Trusted TUI styling
// should be applied only after this boundary.
func Sanitize(s string) string {
	if !stringNeedsSanitizing(s) {
		return s
	}
	return sanitize(s)
}

// SanitizeBytes is the byte-slice form of Sanitize. It returns the original
// slice when no sanitization is needed, avoiding copies in rendering hot paths.
func SanitizeBytes(input []byte) []byte {
	if !bytesNeedSanitizing(input) {
		return input
	}
	return []byte(sanitize(string(input)))
}

func stringNeedsSanitizing(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func bytesNeedSanitizing(input []byte) bool {
	for i := 0; i < len(input); {
		if input[i] < utf8.RuneSelf {
			b := input[i]
			if b != '\n' && b != '\t' && (b < ' ' || b == 0x7f) {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(input[i:])
		if r == utf8.RuneError && size == 1 {
			return true
		}
		if unicode.IsControl(r) {
			return true
		}
		i += size
	}
	return false
}

func sanitize(s string) string {
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
			clean.WriteByte('\n')
		default:
			if !unicode.IsControl(r) {
				clean.WriteRune(r)
			}
		}
	}
	return clean.String()
}

// SanitizeSingleLine removes terminal controls and flattens vertical or tabular
// whitespace so metadata cannot create convincing extra TUI rows.
func SanitizeSingleLine(s string) string {
	s = Sanitize(s)
	s = strings.NewReplacer("\n", " ", "\t", " ").Replace(s)
	return s
}

// EscapeControls renders control characters visibly instead of executing or
// deleting them. Use it where the exact untrusted value is security-relevant,
// such as a shell command awaiting approval or a file path in a diff header.
func EscapeControls(s string) string {
	return escapeControls(s, false)
}

// EscapeControlsPreserveLayout escapes controls but retains newlines and tabs.
// It is intended for structured multi-line text such as diff content.
func EscapeControlsPreserveLayout(s string) string {
	return escapeControls(s, true)
}

func escapeControls(s string, preserveLayout bool) string {
	if s == "" {
		return ""
	}
	var escaped strings.Builder
	escaped.Grow(len(s))
	for _, r := range s {
		if preserveLayout && (r == '\n' || r == '\t') {
			escaped.WriteRune(r)
			continue
		}
		if !unicode.IsControl(r) {
			escaped.WriteRune(r)
			continue
		}
		if r <= 0xff {
			fmt.Fprintf(&escaped, "\\x%02x", r)
		} else {
			fmt.Fprintf(&escaped, "\\u%04x", r)
		}
	}
	return escaped.String()
}
