package secrettext

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DetectorSpec describes a stable owner-selectable built-in. Patterns are
// published so an owner can disable a heuristic and add a narrower alternative.
type DetectorSpec struct{ Name, Pattern, Description string }
type detector struct {
	DetectorSpec
	re     *regexp.Regexp
	groups []int
}

func rule(name, pattern, description string, groups ...int) detector {
	return detector{DetectorSpec{name, pattern, description}, regexp.MustCompile(pattern), groups}
}

const namedKey = `(?:api[ _-]?key|(?:access|auth|refresh|bearer|bot)[ _-]?token|client[ _-]?secret|secret[ _-]?key|private[ _-]?key|password|passwd)`

const cliIdentityPattern = `(?i)(?:^|\s)--?user(?:name)?(?:\s+|=)["']?([^\s"':]+:[^\s"']+)["']?`

var builtins = []detector{
	rule("provider-key", `\b(?:sk|pk|rk)-[A-Za-z0-9_-]{8,}|\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{8,}|\bxox[abeoprs]-[A-Za-z0-9-]{8,}|\bglpat-[A-Za-z0-9_-]{8,}|\bAKIA[0-9A-Z]{12,}|\bAIza[0-9A-Za-z_-]{20,}`, "Recognizable provider key prefixes", 0),
	rule("private-key", `-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----[\s\S]*?(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----|$)`, "PEM private-key blocks", 0),
	rule("auth-header", `(?i)\b(?:proxy-)?authorization["']?\s*[:=]\s*(?:"([^"]*)"|'([^']*)'|((?:(?:basic|bearer|token)\s+)?[^\s"',;}]+))`, "Authorization header values", 1, 2, 3),
	rule("cookie", `(?i)\b(?:set-)?cookie["']?\s*:\s*(?:"([^"]*)"|'([^']*)'|([^"'\n]+))`, "Cookie header values", 1, 2, 3),
	rule("cookie", `\bcurl\b[^|;&\n]*?\s-b\s+(?:"([^"]*)"|'([^']*)'|([^\s"']+))`, "curl cookie arguments", 1, 2, 3),
	rule("cli-user", `\bcurl\b[^|;&\n]*?\s(?:-u|--user)(?:\s+|=)(?:"([^"]*)"|'([^']*)'|([^\s"']+))`, "curl user credentials", 1, 2, 3),
	rule("cli-user", cliIdentityPattern, "User:password CLI arguments (numeric UID:GID is preserved)", 1),
	rule("url-userinfo", `://([^/\s:@]+:[^/\s@]+)@`, "URL userinfo credentials", 1),
	rule("url-param", `(?i)[?&](?:api[_-]?key|access_token|auth_token|token|password|secret)=([^&#\s"']+)`, "Credential-like URL parameters", 1),
	rule("named-credential", `(?i)\b`+namedKey+`["'`+"`"+`*]*\s*[:=]\s*(?:"([^"]*)"|'([^']*)'|`+"`([^`]*)`"+`|([^\r\n,;}&]+))`, "Credential assignments (may also match prose location notes)", 1, 2, 3, 4),
	rule("named-credential", `(?i)(?:^|\s)--`+namedKey+`(?:\s+|=)(?:"([^"]*)"|'([^']*)'|([^\s"']+))`, "Credential CLI options", 1, 2, 3),
	rule("bearer", `(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{16,})`, "Bearer token-shaped values", 1),
}

// BuiltinSpecs returns definitions in stable order, including multiple patterns
// for a detector. Configuring a detector name applies to all of its patterns.
func BuiltinSpecs() []DetectorSpec {
	out := make([]DetectorSpec, len(builtins))
	for i, d := range builtins {
		out[i] = d.DetectorSpec
	}
	return out
}
func Builtins() []string {
	var names []string
	seen := map[string]bool{}
	for _, d := range builtins {
		if !seen[d.Name] {
			names = append(names, d.Name)
			seen[d.Name] = true
		}
	}
	return names
}

// Redactor is immutable. Owner config selects heuristics; text never selects policy.
type Redactor struct{ detectors []detector }

func New(disable, patterns []string) (*Redactor, error) {
	known := map[string]bool{"all": true}
	for _, name := range Builtins() {
		known[name] = true
	}
	off := map[string]bool{}
	for _, name := range disable {
		if !known[name] {
			return nil, fmt.Errorf("unknown memory secret detector %q", name)
		}
		off[name] = true
	}
	r := &Redactor{}
	for _, d := range builtins {
		if !off["all"] && !off[d.Name] {
			r.detectors = append(r.detectors, d)
		}
	}
	for i, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil || re.MatchString("") {
			return nil, fmt.Errorf("invalid memory.secrets.patterns entry %d", i+1)
		}
		r.detectors = append(r.detectors, detector{DetectorSpec{Name: fmt.Sprintf("pattern #%d", i+1)}, re, []int{0}})
	}
	return r, nil
}

var Default = func() *Redactor {
	r, err := New(nil, nil)
	if err != nil {
		panic(err)
	}
	return r
}()

func (r *Redactor) Enabled() bool { return len(r.detectors) > 0 }

type span struct{ start, end int }

func placeholder(text string) bool {
	v := strings.ToLower(strings.TrimSpace(text))
	return v == "" || v == "[redacted]" || v == "redacted" || v == "withheld"
}

var numericUser = regexp.MustCompile(`^\d+:\d+$`)

// Redact unions matches from every detector against ORIGINAL text. Matched
// spans alone are replaced, once; no replacement can hide another rule's context.
func (r *Redactor) Redact(text string) (string, []string) {
	var spans []span
	var hits []string
	seen := map[string]bool{}
	for _, d := range r.detectors {
		for _, m := range d.re.FindAllStringSubmatchIndex(text, -1) {
			for _, group := range d.groups {
				a, b := m[2*group], m[2*group+1]
				if a < 0 || b <= a || (!strings.HasPrefix(d.Name, "pattern #") && placeholder(text[a:b])) {
					continue
				}
				// Numeric UID:GID is benign only in generic identity flags, not curl auth.
				if d.Name == "cli-user" && d.Pattern == cliIdentityPattern && numericUser.MatchString(text[a:b]) {
					continue
				}
				spans = append(spans, span{a, b})
				if !seen[d.Name] {
					hits = append(hits, d.Name)
					seen[d.Name] = true
				}
			}
		}
	}
	if len(spans) == 0 {
		return text, nil
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start == spans[j].start {
			return spans[i].end > spans[j].end
		}
		return spans[i].start < spans[j].start
	})
	merged := spans[:0]
	for _, s := range spans {
		n := len(merged)
		if n > 0 && s.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, s.end)
		} else {
			merged = append(merged, s)
		}
	}
	var out strings.Builder
	offset := 0
	for _, s := range merged {
		out.WriteString(text[offset:s.start])
		out.WriteString("[redacted]")
		offset = s.end
	}
	out.WriteString(text[offset:])
	return out.String(), hits
}

// Redact is the default convenience API, independent of memory owner settings.
func Redact(text string) string { out, _ := Default.Redact(text); return out }
