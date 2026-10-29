package markdown

import (
	"html"
	"regexp"
	"strings"

	"github.com/samsaffron/term-llm/internal/terminaltext"
)

// backslashEscapeRe matches a backslash followed by an ASCII punctuation character.
var backslashEscapeRe = regexp.MustCompile(`\\([!"#$%&'()*+,\-./:;<=>?@\[\\\]^_{|}~` + "`])")

// decodeInlineText processes raw goldmark source text for terminal display:
// 1. Strips CommonMark backslash escapes (\* → *)
// 2. Decodes HTML entities (&amp; → &, &ouml; → ö)
// 3. Sanitizes newly decoded terminal controls before trusted ANSI is added
func decodeInlineText(s string) string {
	if strings.IndexAny(s, "\\&") < 0 {
		return s
	}
	if strings.IndexByte(s, '\\') >= 0 {
		s = backslashEscapeRe.ReplaceAllString(s, "$1")
	}
	if strings.IndexByte(s, '&') >= 0 {
		decoded := html.UnescapeString(s)
		if decoded != s {
			// The source was sanitized before parsing, but entities can introduce
			// fresh controls. Do not sanitize styled output: its ANSI is trusted.
			s = terminaltext.Sanitize(decoded)
		}
	}
	return s
}
