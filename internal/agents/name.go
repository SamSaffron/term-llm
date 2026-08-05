package agents

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLookupNameRunes bounds canonical names used by agent creation and
// unquoted explicit mentions.
const MaxLookupNameRunes = 64

// IsLookupNameAtomRune reports whether r can make up a lookup-name segment.
// Lookup names deliberately exclude sentence punctuation and path separators so
// an explicit name can be recognized without consuming surrounding prose.
func IsLookupNameAtomRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// IsLookupNameSeparatorRune reports whether r may separate non-empty lookup-name
// segments. Spaces are supported through quoted @agent syntax.
func IsLookupNameSeparatorRune(r rune) bool {
	return r == '-' || r == '.' || r == ' '
}

// IsSafeLookupName validates a registry key before it is joined to an agent
// search path. It preserves legacy names accepted by older releases while
// rejecting path traversal and names that are not portable filesystem entries.
func IsSafeLookupName(name string) bool {
	if name == "" || !utf8.ValidString(name) || name == "." || name == ".." || strings.ContainsAny(name, "/\\:*?\"<>|\x00") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// IsLookupName validates the canonical grammar used for newly-created agents
// and unquoted explicit agent mentions. Legacy path-safe names that do not fit
// this grammar remain addressable through quoted mentions and registry lookup.
func IsLookupName(name string) bool {
	if name == "" || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return false
	}
	count := 0
	previousAtom := false
	for _, r := range name {
		count++
		if count > MaxLookupNameRunes {
			return false
		}
		if IsLookupNameAtomRune(r) {
			previousAtom = true
			continue
		}
		if !IsLookupNameSeparatorRune(r) || !previousAtom {
			return false
		}
		previousAtom = false
	}
	return previousAtom
}
