package reasoning

import "strings"

// AppendStreamItemText appends a streamed reasoning delta while preserving the
// paragraph boundary between distinct provider reasoning items. Deltas from the
// same item are appended verbatim because markdown tokens may span deltas.
func AppendStreamItemText(builder *strings.Builder, currentItemID *string, text, itemID string) {
	if text == "" {
		return
	}
	if builder.Len() > 0 && itemID != "" && *currentItemID != "" && itemID != *currentItemID {
		appendParagraphBoundary(builder, text)
	}
	builder.WriteString(text)
	if itemID != "" {
		*currentItemID = itemID
	}
}

func appendParagraphBoundary(builder *strings.Builder, next string) {
	breaks := trailingLineBreaks(builder.String()) + leadingLineBreaks(next)
	for breaks < 2 {
		builder.WriteByte('\n')
		breaks++
	}
}

func trailingLineBreaks(text string) int {
	breaks := 0
	for len(text) > 0 {
		switch text[len(text)-1] {
		case '\n':
			breaks++
			text = text[:len(text)-1]
			if len(text) > 0 && text[len(text)-1] == '\r' {
				text = text[:len(text)-1]
			}
		case '\r':
			breaks++
			text = text[:len(text)-1]
		default:
			return breaks
		}
	}
	return breaks
}

func leadingLineBreaks(text string) int {
	breaks := 0
	for len(text) > 0 {
		switch text[0] {
		case '\r':
			breaks++
			text = text[1:]
			if len(text) > 0 && text[0] == '\n' {
				text = text[1:]
			}
		case '\n':
			breaks++
			text = text[1:]
		default:
			return breaks
		}
	}
	return breaks
}
