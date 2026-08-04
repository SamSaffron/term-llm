package mentions

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/agents"
)

const agentMentionPrefix = "agent:"

// SubmittedAgentMention is an explicit textual delegation request. Start and
// End are byte offsets covering the complete visible @agent token, excluding
// ordinary punctuation that terminates an unquoted name.
type SubmittedAgentMention struct {
	Name       string
	Start, End int
}

// ParseSubmittedAgents extracts deliberate, well-formed @agent:<lookup-name>
// and @agent:"lookup name" tokens in textual order. The bounded lookup grammar
// is shared with the actual agent registry. Unknown valid names are returned so
// runtime policy can reject them; malformed agent-like prose is ignored.
func ParseSubmittedAgents(text string) ([]SubmittedAgentMention, error) {
	var result []SubmittedAgentMention
	for offset := 0; offset < len(text); {
		at := strings.IndexByte(text[offset:], '@')
		if at < 0 {
			break
		}
		at += offset
		offset = at + 1
		if at > 0 {
			previous, _ := utf8.DecodeLastRuneInString(text[:at])
			if !isMentionBoundary(previous) {
				continue
			}
		}
		payloadStart := at + 1
		if !strings.HasPrefix(text[payloadStart:], agentMentionPrefix) {
			continue
		}
		nameStart := payloadStart + len(agentMentionPrefix)
		if nameStart >= len(text) {
			continue
		}

		var mention SubmittedAgentMention
		var ok bool
		if text[nameStart] == '"' {
			mention, ok = parseQuotedAgentMention(text, at, nameStart+1)
		} else {
			mention, ok = parseUnquotedAgentMention(text, at, nameStart)
		}
		if !ok {
			continue
		}
		result = append(result, mention)
		offset = mention.End
	}
	return result, nil
}

func parseUnquotedAgentMention(text string, at, start int) (SubmittedAgentMention, bool) {
	scanEnd := start
	lastAtomEnd := start
	for scanEnd < len(text) {
		r, size := utf8.DecodeRuneInString(text[scanEnd:])
		if agents.IsLookupNameAtomRune(r) {
			scanEnd += size
			lastAtomEnd = scanEnd
			continue
		}
		if r == '-' || r == '.' {
			scanEnd += size
			continue
		}
		break
	}
	if lastAtomEnd == start {
		return SubmittedAgentMention{}, false
	}
	name := text[start:lastAtomEnd]
	if !agents.IsLookupName(name) {
		return SubmittedAgentMention{}, false
	}
	// Separators after the final segment are punctuation, not part of the name.
	// Any other non-prose suffix (for example /, #, or a stray quote) makes the
	// token malformed rather than silently delegating to its valid prefix.
	if scanEnd < len(text) {
		next, _ := utf8.DecodeRuneInString(text[scanEnd:])
		if !isAgentMentionTerminator(next) {
			return SubmittedAgentMention{}, false
		}
	}
	return SubmittedAgentMention{Name: name, Start: at, End: lastAtomEnd}, true
}

func parseQuotedAgentMention(text string, at, start int) (SubmittedAgentMention, bool) {
	var name strings.Builder
	escaped := false
	for end := start; end < len(text); {
		r, size := utf8.DecodeRuneInString(text[end:])
		end += size
		if escaped {
			name.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r != '"' {
			name.WriteRune(r)
			continue
		}
		value := name.String()
		if !agents.IsLookupName(value) {
			return SubmittedAgentMention{}, false
		}
		if end < len(text) {
			next, _ := utf8.DecodeRuneInString(text[end:])
			if !isAgentMentionTerminator(next) {
				return SubmittedAgentMention{}, false
			}
		}
		return SubmittedAgentMention{Name: value, Start: at, End: end}, true
	}
	return SubmittedAgentMention{}, false
}

func isAgentMentionTerminator(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	return strings.ContainsRune(",.;:!?)]}。、？！", r)
}

// UniqueAgentMentionNames deduplicates canonical names in first-occurrence
// order without changing the submitted text.
func UniqueAgentMentionNames(mentions []SubmittedAgentMention) []string {
	names := make([]string, 0, len(mentions))
	seen := make(map[string]struct{}, len(mentions))
	for _, mention := range mentions {
		if _, duplicate := seen[mention.Name]; duplicate {
			continue
		}
		seen[mention.Name] = struct{}{}
		names = append(names, mention.Name)
	}
	return names
}

// InsertAgentText returns the canonical visible delegation token.
func InsertAgentText(name string) string {
	if strings.ContainsAny(name, " \t\r\n\"\\") {
		escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(name)
		return `@agent:"` + escaped + `"`
	}
	return "@agent:" + name
}

func isReservedAgentFilePayload(payload string) bool {
	return strings.HasPrefix(payload, agentMentionPrefix)
}
