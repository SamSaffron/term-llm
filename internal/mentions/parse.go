package mentions

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/agents"
)

var (
	submittedLineRange = regexp.MustCompile(`#L([1-9][0-9]*)(?:-L?([1-9][0-9]*))?$`)
	submittedPathParts = regexp.MustCompile(`^([^#]+)(?:(#L[1-9][0-9]*(?:-L?[1-9][0-9]*)?))?(?:#[^#]*)?$`)
)

// ActiveTokenAt returns the @ token containing the byte cursor. Tokens start
// at the buffer beginning or after whitespace or one of Claude Code's four
// CJK/fullwidth sentence delimiters. Quoted tokens allow spaces and backslash
// escapes.
func ActiveTokenAt(text string, cursor int) (ActiveToken, bool) {
	if cursor < 0 || cursor > len(text) || !utf8.ValidString(text) {
		return ActiveToken{}, false
	}
	lineStart := strings.LastIndexByte(text[:cursor], '\n') + 1
	for start := cursor - 1; start >= lineStart; start-- {
		if text[start] != '@' {
			continue
		}
		if start > 0 {
			r, _ := utf8.DecodeLastRuneInString(text[:start])
			if !isMentionBoundary(r) {
				continue
			}
		}
		tail := text[start+1 : cursor]
		if strings.HasPrefix(tail, `agent:"`) {
			query, ok := parseQuotedTail(tail[len(`agent:"`):])
			if !ok {
				continue
			}
			return ActiveToken{Start: start, End: cursor, Query: query, Quoted: true, Agent: true}, true
		}
		if strings.HasPrefix(tail, "\"") {
			query, ok := parseQuotedTail(tail[1:])
			if !ok {
				continue
			}
			return ActiveToken{Start: start, End: cursor, Query: query, Quoted: true}, true
		}
		if strings.HasPrefix(tail, agentMentionPrefix) {
			query := strings.TrimPrefix(tail, agentMentionPrefix)
			if strings.ContainsFunc(query, func(r rune) bool {
				return !agents.IsLookupNameAtomRune(r) && r != '-' && r != '.'
			}) {
				continue
			}
			return ActiveToken{Start: start, End: cursor, Query: query, Agent: true}, true
		}
		if strings.ContainsAny(tail, " \t\r\n\"") {
			continue
		}
		return ActiveToken{Start: start, End: cursor, Query: tail}, true
	}
	return ActiveToken{}, false
}

func parseQuotedTail(tail string) (string, bool) {
	var b strings.Builder
	escaped := false
	for _, r := range tail {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			return "", false // a closed quoted token is no longer active
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteRune('\\')
	}
	return b.String(), true
}

func isMentionBoundary(r rune) bool {
	return unicode.IsSpace(r) || r == '。' || r == '、' || r == '？' || r == '！'
}

// InsertText returns a visible mention token. Paths needing spaces or quoting
// are escaped using the @"..." form.
func InsertText(path string, directory bool) string {
	// The unforced @agent: namespace is reserved for delegation. Prefix a real
	// colliding project path so submit-time parsing still treats it as a file.
	if isReservedAgentFilePayload(path) {
		path = "./" + path
	}
	if directory && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	if strings.ContainsAny(path, " \t\"\\") {
		escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(path)
		return "@\"" + escaped + "\""
	}
	return "@" + path
}

// ParseSubmitted extracts file and directory mentions at Claude Code's mention
// start boundaries. The four CJK/fullwidth punctuation runes are start
// delimiters only: unquoted payloads still continue until whitespace. Quoted
// matches are returned before unquoted matches, and duplicate raw payloads are
// removed before path parsing. Quoted paths may contain spaces and escaped
// quotes/backslashes. A trailing #Lx or #Lx-y suffix inside the quoted or
// unquoted payload selects one-based lines.
func ParseSubmitted(text string) []SubmittedMention {
	matches := append(collectSubmittedMatches(text, true), collectSubmittedMatches(text, false)...)
	mentions := make([]SubmittedMention, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if _, duplicate := seen[match.raw]; duplicate {
			continue
		}
		seen[match.raw] = struct{}{}
		mentions = append(mentions, match.mention)
	}
	return mentions
}

type submittedMatch struct {
	mention SubmittedMention
	raw     string
	end     int
	quoted  bool
}

func collectSubmittedMatches(text string, quoted bool) []submittedMatch {
	var matches []submittedMatch
	for offset := 0; offset < len(text); {
		at := strings.IndexByte(text[offset:], '@')
		if at < 0 {
			break
		}
		at += offset
		if at > 0 {
			previous, _ := utf8.DecodeLastRuneInString(text[:at])
			if !isMentionBoundary(previous) {
				offset = at + 1
				continue
			}
		}

		match, ok := parseSubmittedAt(text, at)
		if !ok || match.quoted != quoted {
			offset = at + 1
			continue
		}
		offset = max(match.end, at+1)
		matches = append(matches, match)
	}
	return matches
}

func parseSubmittedAt(text string, at int) (submittedMatch, bool) {
	start := at + 1
	if start >= len(text) {
		return submittedMatch{}, false
	}
	if isReservedAgentFilePayload(text[start:]) {
		return submittedMatch{}, false
	}
	if text[start] != '"' {
		end := start
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			if unicode.IsSpace(r) {
				break
			}
			end += size
		}
		if end == start {
			return submittedMatch{}, false
		}
		raw := text[start:end]
		mention := parseSubmittedPath(raw)
		return submittedMatch{mention: mention, raw: raw, end: end}, mention.Path != ""
	}

	var path strings.Builder
	escaped := false
	end := start + 1
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		end += size
		if escaped {
			path.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			raw := path.String()
			mention := parseSubmittedPath(raw)
			return submittedMatch{mention: mention, raw: raw, end: end, quoted: true}, mention.Path != ""
		}
		path.WriteRune(r)
	}
	return submittedMatch{}, false
}

func parseSubmittedPath(value string) SubmittedMention {
	parts := submittedPathParts.FindStringSubmatch(value)
	if parts == nil {
		return SubmittedMention{Path: value}
	}
	mention := SubmittedMention{Path: parts[1]}
	if parts[2] == "" {
		return mention
	}
	start, end, ok := parseLineRange(parts[2])
	if !ok {
		// Do not turn an invalid range into an unintended whole-file read.
		return SubmittedMention{Path: value}
	}
	mention.LineStart, mention.LineEnd = start, end
	return mention
}

func parseLineRange(value string) (int, int, bool) {
	match := submittedLineRange.FindStringSubmatch(value)
	if match == nil {
		return 0, 0, false
	}
	start, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, false
	}
	end := start
	if match[2] != "" {
		end, err = strconv.Atoi(match[2])
		if err != nil || end < start {
			return 0, 0, false
		}
	}
	return start, end, true
}
