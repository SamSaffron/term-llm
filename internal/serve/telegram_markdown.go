package serve

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"
)

// telegramMarkdown is a shared goldmark instance with the strikethrough extension.
var telegramMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Strikethrough),
)

// mdToTelegramHTML converts Markdown text to Telegram-compatible HTML.
//
// Telegram's Bot API supports a limited HTML subset:
//
//	<b>, <strong>, <i>, <em>, <u>, <ins>, <s>, <strike>, <del>,
//	<code>, <pre>, <a href>, <blockquote>, <tg-spoiler>
//
// Everything else is mapped or stripped.
func mdToTelegramHTML(md string) string {
	if strings.TrimSpace(md) == "" {
		return md
	}

	var htmlBuf bytes.Buffer
	if err := telegramMarkdown.Convert([]byte(md), &htmlBuf); err != nil {
		// Fallback: return escaped plain text.
		return html.EscapeString(md)
	}

	return htmlToTelegram(htmlBuf.String())
}

// htmlToTelegram walks a goldmark HTML output and produces Telegram-safe HTML.
func htmlToTelegram(src string) string {
	z := html.NewTokenizer(strings.NewReader(src))

	var sb strings.Builder
	// Track list state for ordered lists.
	type listState struct {
		ordered bool
		counter int
	}
	var listStack []listState

	inPre := false // inside <pre>; suppress inner <code> tags but keep text

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}

		tok := z.Token()

		switch tt {
		case html.TextToken:
			// Token() decodes HTML entities, including inside preformatted blocks.
			// Escape text again while leaving explicitly generated tags intact.
			sb.WriteString(html.EscapeString(tok.Data))

		case html.StartTagToken, html.SelfClosingTagToken:
			tag := tok.Data
			switch tag {
			case "b", "strong":
				sb.WriteString("<b>")
			case "i", "em":
				sb.WriteString("<i>")
			case "u", "ins":
				sb.WriteString("<u>")
			case "s", "strike", "del":
				sb.WriteString("<s>")
			case "code":
				if !inPre {
					sb.WriteString("<code>")
				}
			case "pre":
				inPre = true
				sb.WriteString("<pre>")
			case "a":
				href := attrVal(tok.Attr, "href")
				if href != "" {
					fmt.Fprintf(&sb, `<a href="%s">`, html.EscapeString(href))
				} else {
					sb.WriteString("<a>")
				}
			case "blockquote":
				sb.WriteString("<blockquote>")
			case "p":
				// Paragraphs: no tag, but we'll add newlines on close.
			case "br":
				sb.WriteString("\n")
			case "ul":
				listStack = append(listStack, listState{ordered: false})
			case "ol":
				listStack = append(listStack, listState{ordered: true, counter: 0})
			case "li":
				if len(listStack) > 0 {
					top := &listStack[len(listStack)-1]
					if top.ordered {
						top.counter++
						fmt.Fprintf(&sb, "\n%d. ", top.counter)
					} else {
						sb.WriteString("\n• ")
					}
				} else {
					sb.WriteString("\n• ")
				}
			case "h1", "h2", "h3", "h4", "h5", "h6":
				sb.WriteString("<b>")
			case "hr":
				sb.WriteString("\n──────────\n")
				// All other tags: silently ignore (don't emit the tag).
			}

		case html.EndTagToken:
			tag := tok.Data
			switch tag {
			case "b", "strong":
				sb.WriteString("</b>")
			case "i", "em":
				sb.WriteString("</i>")
			case "u", "ins":
				sb.WriteString("</u>")
			case "s", "strike", "del":
				sb.WriteString("</s>")
			case "code":
				if !inPre {
					sb.WriteString("</code>")
				}
			case "pre":
				inPre = false
				sb.WriteString("</pre>")
			case "a":
				sb.WriteString("</a>")
			case "blockquote":
				sb.WriteString("</blockquote>")
			case "p":
				sb.WriteString("\n\n")
			case "ul", "ol":
				if len(listStack) > 0 {
					listStack = listStack[:len(listStack)-1]
				}
				sb.WriteString("\n")
			case "li":
				// nothing
			case "h1", "h2", "h3", "h4", "h5", "h6":
				sb.WriteString("</b>\n\n")
			}
		}
	}

	// Clean up: trim trailing whitespace and collapse excessive blank lines.
	result := strings.TrimSpace(sb.String())
	// Collapse 3+ consecutive newlines to 2.
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return result
}

// attrVal returns the value of a named HTML attribute, or "".
func attrVal(attrs []html.Attribute, name string) string {
	for _, a := range attrs {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
