package linkeddevices

import (
	"os"
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

// serverHostLabel names this server on the devices it pairs with: the
// configured node label, then the agent, then the machine's hostname, so an
// invitation always carries a name.
func serverHostLabel(label, agent string) string {
	if clean := safeHostLabel(label, agent); clean != "" {
		return clean
	}
	host, _ := os.Hostname()
	return safeHostLabel(host, "")
}
