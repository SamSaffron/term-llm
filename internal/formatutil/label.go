package formatutil

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxDisplayLabelRunes bounds a short human-readable display label such as the
// title of a queued job in conversation activity.
const MaxDisplayLabelRunes = 76

// NormalizeDisplayLabel collapses whitespace, drops control characters and
// returns "" when the value cannot be a short, single-line display label.
// Mentions are rejected so a label can never address an agent or user.
func NormalizeDisplayLabel(label string) string {
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, label)
	label = strings.Join(strings.Fields(label), " ")
	if label == "" || utf8.RuneCountInString(label) > MaxDisplayLabelRunes || strings.Contains(label, "@") {
		return ""
	}
	return label
}

// ValidDisplayLabel reports whether an optional label is absent or usable as
// one. A blank label is valid: the caller simply has nothing to show.
func ValidDisplayLabel(label string) error {
	if strings.TrimSpace(label) == "" || NormalizeDisplayLabel(label) != "" {
		return nil
	}
	return fmt.Errorf("label must be a single-line description of at most %d characters without @mentions", MaxDisplayLabelRunes)
}
