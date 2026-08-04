package agents

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLookupNameRunes bounds exact registry lookup keys used by agent selection,
// spawn policy, and explicit @agent mentions.
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

// IsLookupName validates an exact registry lookup key. The same grammar is used
// by registry discovery, spawn execution, and explicit @agent parsing.
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
