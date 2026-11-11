package secrettext

import (
	"regexp"
	"strings"
)

// credentialPatterns are shared with the native live-preview redactor. They
// recognize common credential contexts without treating arbitrary long hashes
// as secrets. This remains a conservative backstop, not a guarantee.
var credentialPatterns = []struct {
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

var privateKey = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----[\s\S]*?(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----|$)`)

// Exact key names avoid treating token counts, git authors or commit hashes as
// credentials. These additional text rules complement the existing native
// preview rules above; they do not inspect process environment or execute code.
const namedKey = `(?:api[ _-]?key|(?:access|auth|refresh|bearer|bot)[ _-]?token|client[ _-]?secret|secret[ _-]?key|private[ _-]?key|password|passwd)`

var namedValues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(\b` + namedKey + `["'` + "`" + `*]*\s*[:=]\s*)("[^"]*"|'[^']*'|` + "`[^`]*`" + `|[^\r\n,;}&]+)`),
	regexp.MustCompile(`(?i)((?:^|\s)--` + namedKey + `(?:\s+|=))("[^"]*"|'[^']*'|[^\s"']+)`),
}
var queryValue = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|access_token|auth_token|token|password|secret)=)([^&#\s"']+)`)
var bearer = regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{16,})`)

func placeholder(value string) bool {
	value = strings.Trim(strings.TrimSpace(value), "\"'`")
	lower := strings.ToLower(value)
	return value == "" || lower == "[redacted]" || lower == "redacted" || lower == "withheld"
}

// Redact preserves formatting except for recognized credential spans. It runs
// on complete source text before any clipping so the detection context survives.
func Redact(text string) string {
	text = privateKey.ReplaceAllString(text, "[redacted]")
	for _, secret := range credentialPatterns {
		text = secret.pattern.ReplaceAllString(text, secret.replacement)
	}
	for _, re := range namedValues {
		text = re.ReplaceAllStringFunc(text, func(match string) string {
			parts := re.FindStringSubmatch(match)
			if placeholder(parts[2]) {
				return match
			}
			return parts[1] + "[redacted]"
		})
	}
	text = queryValue.ReplaceAllStringFunc(text, func(match string) string {
		p := queryValue.FindStringSubmatch(match)
		if placeholder(p[2]) {
			return match
		}
		return p[1] + "[redacted]"
	})
	return bearer.ReplaceAllStringFunc(text, func(match string) string {
		p := bearer.FindStringSubmatch(match)
		if placeholder(p[2]) {
			return match
		}
		return p[1] + "[redacted]"
	})
}
