package mentions

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const agentMentionPrefix = "agent:"

// SubmittedAgentMention is an explicit textual delegation request. Start and
// End are byte offsets covering the complete visible @agent token.
type SubmittedAgentMention struct {
	Name       string
	Start, End int
}

// AgentMentionSyntaxError describes a malformed explicit @agent token.
type AgentMentionSyntaxError struct {
	Offset  int
	Message string
}

func (e *AgentMentionSyntaxError) Error() string {
	return fmt.Sprintf("invalid @agent mention at byte %d: %s", e.Offset, e.Message)
}

// ParseSubmittedAgents extracts explicit @agent:<lookup-name> and
// @agent:"name with spaces" tokens in textual order. Bare @name remains file or
// ordinary text syntax. Duplicates are returned so visible occurrences retain
// their source offsets; UniqueAgentMentionNames performs canonical deduplication.
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
			return nil, &AgentMentionSyntaxError{Offset: at, Message: "expected an agent lookup name after @agent:"}
		}
		if text[nameStart] == '"' {
			name, end, err := parseQuotedAgentName(text, at, nameStart+1)
			if err != nil {
				return nil, err
			}
			result = append(result, SubmittedAgentMention{Name: name, Start: at, End: end})
			offset = end
			continue
		}

		end := nameStart
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			if unicode.IsSpace(r) {
				break
			}
			end += size
		}
		name := text[nameStart:end]
		if name == "" {
			return nil, &AgentMentionSyntaxError{Offset: at, Message: "expected an agent lookup name after @agent:"}
		}
		if strings.ContainsRune(name, '"') {
			return nil, &AgentMentionSyntaxError{Offset: at, Message: "quotes must surround the complete agent lookup name"}
		}
		result = append(result, SubmittedAgentMention{Name: name, Start: at, End: end})
		offset = end
	}
	return result, nil
}

func parseQuotedAgentName(text string, at, start int) (string, int, error) {
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
		if r == '"' {
			if name.Len() == 0 {
				return "", 0, &AgentMentionSyntaxError{Offset: at, Message: "quoted agent lookup name cannot be empty"}
			}
			if end < len(text) {
				next, _ := utf8.DecodeRuneInString(text[end:])
				if !isMentionBoundary(next) {
					return "", 0, &AgentMentionSyntaxError{Offset: at, Message: "unexpected text after quoted agent lookup name"}
				}
			}
			return name.String(), end, nil
		}
		name.WriteRune(r)
	}
	return "", 0, &AgentMentionSyntaxError{Offset: at, Message: "unterminated quoted agent lookup name"}
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
