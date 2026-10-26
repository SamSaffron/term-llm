package linkeddevices

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxHostLabelBytes = 64

func safeHostLabel(label, fallback string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, strings.TrimSpace(label))
	if clean == "" {
		clean = fallback
	}
	for len(clean) > maxHostLabelBytes {
		_, size := utf8.DecodeLastRuneInString(clean)
		clean = clean[:len(clean)-size]
	}
	return strings.TrimSpace(clean)
}
