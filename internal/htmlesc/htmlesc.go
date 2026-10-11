// Package htmlesc provides the small set of contextual HTML escapers that
// term-llm's hand-written HTML renderers need.
//
// term-llm deliberately avoids html/template and text/template in production
// code. Template execution resolves fields with reflect.Value.MethodByName,
// and once the linker sees that call reachable it must keep every exported
// method of every type that can be converted to an interface. With large
// generated SDKs linked in, that disables most dead-code elimination.
//
// The escapers below reproduce html/template's output for the contexts we use,
// so pages render byte-for-byte as they did when they were templates.
package htmlesc

import (
	"fmt"
	"strings"
)

// replacer mirrors html/template's htmlReplacementTable. Every replaced byte
// is ASCII, so byte-wise replacement matches html/template's rune-wise loop
// even on invalid UTF-8.
var replacer = strings.NewReplacer(
	"\x00", "\uFFFD",
	`"`, "&#34;",
	"&", "&amp;",
	"'", "&#39;",
	"+", "&#43;",
	"<", "&lt;",
	">", "&gt;",
)

// Text escapes s for element content (including RCDATA such as <title>) and
// for quoted, non-URL attribute values.
func Text(s string) string {
	return replacer.Replace(s)
}

// URL escapes an untrusted value that forms an entire quoted URL attribute
// (href, src, action). Values with a scheme other than http, https or mailto
// are replaced with "#ZgotmplZ", exactly as html/template does.
func URL(s string) string {
	if !isSafeURL(s) {
		return "#ZgotmplZ"
	}
	return TrustedURL(s)
}

// TrustedURL escapes a URL that the caller has already validated (the
// equivalent of passing html/template a template.URL value). The scheme is not
// filtered; the URL is normalized and attribute-escaped.
func TrustedURL(s string) string {
	return Text(processURL(s, true))
}

// URLComponent escapes a value interpolated into the query or fragment part of
// a quoted URL attribute, such as href="#{{id}}".
func URLComponent(s string) string {
	return Text(processURL(s, false))
}

func isSafeURL(s string) bool {
	if protocol, _, ok := strings.Cut(s, ":"); ok && !strings.Contains(protocol, "/") {
		if !strings.EqualFold(protocol, "http") && !strings.EqualFold(protocol, "https") && !strings.EqualFold(protocol, "mailto") {
			return false
		}
	}
	return true
}

// processURL mirrors html/template's processURLOnto: norm keeps reserved
// characters and valid percent escapes, otherwise only unreserved characters
// pass through unencoded.
func processURL(s string, norm bool) string {
	var b strings.Builder
	written := 0
	for i, n := 0, len(s); i < n; i++ {
		c := s[i]
		switch c {
		case '!', '#', '$', '&', '*', '+', ',', '/', ':', ';', '=', '?', '@', '[', ']':
			if norm {
				continue
			}
		case '-', '.', '_', '~':
			continue
		case '%':
			if norm && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
				continue
			}
		default:
			if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
				continue
			}
		}
		if written == 0 {
			b.Grow(len(s) + 16)
		}
		b.WriteString(s[written:i])
		fmt.Fprintf(&b, "%%%02x", c)
		written = i + 1
	}
	if written == 0 {
		return s
	}
	b.WriteString(s[written:])
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
