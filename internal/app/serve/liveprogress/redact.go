package liveprogress

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// liveSecretPatterns catch credentials a tool preview can carry, such as a
// shell command with an auth header or a key in a URL. Previews are sent to the
// voice provider, and the model may repeat them aloud. This is a conservative
// backstop, not a guarantee.
var liveSecretPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// Whatever follows an Authorization or Cookie header name, quoted or not.
	{regexp.MustCompile(`(?i)\b(authorization["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|(?:(?:basic|bearer|token)\s+)?[^\s"',;}]+)`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)\b((?:set-)?cookie["']?\s*:\s*)(?:"[^"]*"|'[^']*'|[^"'\n]+)`), "${1}[redacted]"},
	// curl's -b sends cookies. Only curl's: -b means something else elsewhere
	// (git checkout -b names a branch). --cookie is caught by the flag rule.
	{regexp.MustCompile(`(\bcurl\b[^|;&\n]*?\s-b\s+)(?:"[^"]*"|'[^']*'|[^\s"']+)`), "${1}[redacted]"},
	// curl's -u/--user carries user:password; elsewhere, only a --user value
	// shaped like user:password is treated as a credential.
	{regexp.MustCompile(`(\bcurl\b[^|;&\n]*?\s(?:-u|--user)(?:\s+|=))(?:"[^"]*"|'[^']*'|[^\s"']+)`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)((?:^|\s)--?user(?:name)?(?:\s+|=))["']?[^\s"':]+:[^\s"']+["']?`), "${1}[redacted]"},
	// user:password@ in URLs.
	{regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`), "://[redacted]@"},
	// Well-known key prefixes, however short the rest.
	{regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{8,}|\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{8,}|\bxox[abeoprs]-[A-Za-z0-9-]{8,}|\bglpat-[A-Za-z0-9_-]{8,}|\bAKIA[0-9A-Z]{12,}|\bAIza[0-9A-Za-z_-]{20,}`), "[redacted]"},
}

// liveSecretKey matches a field or flag name that may hold a secret;
// isLiveSecretKey then rejects the few names that only look like one.
const liveSecretKey = `[a-z0-9_.-]*(?:token|secret|passw(?:or)?d|pwd|api[_-]?key|access[_-]?key|private[_-]?key|auth|credential|cookie)[a-z0-9_.-]*`

// liveKeyedSecrets match a value named by its key: key=value, key: value and
// "key": "value", then --flag value. Group 1 is kept, group 2 is the key.
var liveKeyedSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)((["']?)\b(` + liveSecretKey + `)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s"'&,;}]+)`),
	regexp.MustCompile(`(?i)((^|\s)--?(` + liveSecretKey + `)\s+)(?:"[^"]*"|'[^']*'|[^\s"'-][^\s"']*)`),
}

var liveSecretKeyExact = regexp.MustCompile(`^` + liveSecretKey + `$`)

// isLiveSecretKey rejects names that match only through "author", such as
// git's --author or co-author, while --author-token still names a secret.
func isLiveSecretKey(key string) bool {
	key = strings.ToLower(key)
	if !strings.Contains(key, "authoriz") {
		key = strings.ReplaceAll(key, "author", "")
	}
	return liveSecretKeyExact.MatchString(key)
}

// liveBearer matches a bearer credential, quoted or bare. Group 1 is kept.
var liveBearer = regexp.MustCompile(`(?i)\b(bearer\s+["']?)([A-Za-z0-9._~+/=-]+)`)

// credentialShaped reports whether a bearer value looks like a credential
// rather than the next word of prose ("fix bearer handling"): it has a digit or
// symbol, mixed case after its first letter, or is long.
func credentialShaped(value string) bool {
	if len(value) >= 16 || strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) }) {
		return true
	}
	_, size := utf8.DecodeRuneInString(value)
	rest := value[size:]
	return strings.ContainsFunc(rest, unicode.IsUpper) && strings.ContainsFunc(rest, unicode.IsLower)
}

// liveLongToken matches opaque runs long enough to be a credential. Slashes and
// dots are excluded so ordinary paths and file names survive.
var liveLongToken = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)

func redactLiveProgress(text string) string {
	for _, secret := range liveSecretPatterns {
		text = secret.pattern.ReplaceAllString(text, secret.replacement)
	}
	for _, keyed := range liveKeyedSecrets {
		text = keyed.ReplaceAllStringFunc(text, func(match string) string {
			parts := keyed.FindStringSubmatch(match)
			if !isLiveSecretKey(parts[3]) {
				return match
			}
			return parts[1] + "[redacted]"
		})
	}
	text = liveBearer.ReplaceAllStringFunc(text, func(match string) string {
		parts := liveBearer.FindStringSubmatch(match)
		if credentialShaped(parts[2]) {
			return parts[1] + "[redacted]"
		}
		return match
	})
	return liveLongToken.ReplaceAllStringFunc(text, func(token string) string {
		// Identifiers such as test names are long too; only a mix of letters and
		// digits looks like a key or hash.
		if strings.ContainsFunc(token, unicode.IsDigit) && strings.ContainsFunc(token, unicode.IsLetter) {
			return "[redacted]"
		}
		return token
	})
}

// liveProgressText reduces event text to one bounded line: credential-like
// tokens redacted, control characters dropped, whitespace collapsed, double
// quotes swapped so a quoted field keeps its boundary, and the result cut on a
// rune boundary with an ellipsis.
func liveProgressText(text string, limit int) string {
	text = redactLiveProgress(text)
	var b strings.Builder
	space := false
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case !unicode.IsPrint(r):
			continue
		case r == '"':
			r = '\''
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return truncateProgress(b.String(), limit)
}

// truncateProgress cuts text to at most limit bytes on a rune boundary, marking
// the cut with an ellipsis.
func truncateProgress(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const ellipsis = "…"
	marker := ellipsis
	if limit < len(ellipsis) {
		marker = ""
	}
	cut := max(limit-len(marker), 0)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + marker
}
