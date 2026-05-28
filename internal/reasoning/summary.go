package reasoning

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxReasoningTitleRunes = 80

// ParsedReasoningSummary is the display-oriented shape of provider-sanctioned
// reasoning summary text.
type ParsedReasoningSummary struct {
	Title string
	Body  string
}

// ParseReasoningSummary extracts an optional leading bold title block from a
// displayable reasoning summary. It recognizes Markdown of the form:
//
//	**Title**
//
//	Body
//
// and the streaming partial form "**Title**". Ordinary inline bold prefixes
// such as "**Important:** keep this" are left untouched as body text.
func ParseReasoningSummary(text string) ParsedReasoningSummary {
	trimmed := strings.TrimSpace(normalizeLineEndings(text))
	if trimmed == "" {
		return ParsedReasoningSummary{}
	}

	if !strings.HasPrefix(trimmed, "**") {
		return ParsedReasoningSummary{Body: trimmed}
	}

	closing := strings.Index(trimmed[2:], "**")
	if closing < 0 {
		return ParsedReasoningSummary{Body: trimmed}
	}
	closing += 2

	rawTitle := trimmed[2:closing]
	after := trimmed[closing+2:]
	if rawTitle == "" || strings.Contains(rawTitle, "\n") {
		return ParsedReasoningSummary{Body: trimmed}
	}

	// A title block must end the first line. If prose follows on the same line,
	// this is ordinary bold inline text, not a title.
	if after != "" && !strings.HasPrefix(after, "\n") {
		if strings.TrimSpace(untilNewline(after)) != "" {
			return ParsedReasoningSummary{Body: trimmed}
		}
	}

	title := sanitizeTitle(rawTitle)
	if title == "" || looksLikeInlineBoldPrefix(title) {
		return ParsedReasoningSummary{Body: trimmed}
	}

	body := ""
	if after != "" {
		body = after
		if strings.HasPrefix(body, "\n") {
			body = body[1:]
		}
		// Common title block form has one blank line between title and body.
		if strings.HasPrefix(body, "\n") {
			body = body[1:]
		}
		body = strings.TrimRight(body, " \t\n")
	}

	return ParsedReasoningSummary{Title: title, Body: body}
}

func normalizeLineEndings(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text
}

func untilNewline(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func sanitizeTitle(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	})
	if len(fields) == 0 {
		return ""
	}
	title := strings.Join(fields, " ")
	return truncateRunes(title, MaxReasoningTitleRunes)
}

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= max-1 {
			break
		}
		b.WriteRune(r)
		count++
	}
	b.WriteRune('…')
	return b.String()
}

func looksLikeInlineBoldPrefix(title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return true
	}
	last, _ := utf8.DecodeLastRuneInString(title)
	// Headings/titles can contain punctuation internally, but a terminal colon is
	// very often an inline label ("Important:", "Note:") rather than a summary
	// title. Other punctuation remains allowed for legitimate titles.
	return last == ':'
}
