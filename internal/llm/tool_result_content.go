package llm

import (
	"encoding/base64"
	"fmt"
	"strings"
)

func toolResultContentParts(result *ToolResult) []ToolContentPart {
	if result == nil {
		return nil
	}
	if len(result.ContentParts) > 0 {
		return result.ContentParts
	}
	if result.Content == "" {
		return nil
	}
	return []ToolContentPart{{
		Type: ToolContentPartText,
		Text: result.Content,
	}}
}

func toolResultTextContent(result *ToolResult) string {
	if result == nil {
		return ""
	}
	if len(result.ContentParts) == 0 {
		return result.Content
	}

	var b strings.Builder
	for _, part := range result.ContentParts {
		if part.Type == ToolContentPartText && part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	if b.Len() == 0 {
		return result.Content
	}
	return b.String()
}

// truncateToolOutput applies a single aggregate text limit across structured
// content parts. Images and part ordering are preserved, while the retained
// head and tail text use the same marker as plain tool output truncation.
func truncateToolOutput(output ToolOutput, maxChars int) ToolOutput {
	content, parts := truncateToolResultContent(output.Content, output.ContentParts, maxChars)
	output.Content = content
	output.ContentParts = parts
	return output
}

func truncateToolResultContent(content string, parts []ToolContentPart, maxChars int) (string, []ToolContentPart) {
	if len(parts) == 0 {
		return TruncateToolResult(content, maxChars), nil
	}

	parts = cloneToolContentParts(parts)
	var text strings.Builder
	for _, part := range parts {
		if part.Type == ToolContentPartText {
			text.WriteString(part.Text)
		}
	}

	textRunes := []rune(text.String())
	if len(textRunes) == 0 {
		// Image-only structured results may still carry a textual fallback.
		return TruncateToolResult(content, maxChars), parts
	}
	if len(textRunes) <= maxChars {
		// Structured text is canonical. Do not retain a divergent, potentially
		// unbounded second copy in Content.
		return string(textRunes), parts
	}

	head := maxChars / 2
	tail := maxChars - head
	middle := string(textRunes[head : len(textRunes)-tail])
	marker := fmt.Sprintf("\n[...%d chars truncated - %d lines...]\n", len(textRunes)-maxChars, 1+strings.Count(middle, "\n"))
	tailStart := len(textRunes) - tail
	textOffset := 0
	markerAdded := false

	for i := range parts {
		if parts[i].Type != ToolContentPartText {
			continue
		}
		runes := []rune(parts[i].Text)
		start := textOffset
		end := start + len(runes)
		var retained strings.Builder

		if start < head {
			prefixEnd := min(end, head) - start
			retained.WriteString(string(runes[:prefixEnd]))
		}
		if !markerAdded && end >= head {
			retained.WriteString(marker)
			markerAdded = true
		}
		if end > tailStart {
			suffixStart := max(start, tailStart) - start
			retained.WriteString(string(runes[suffixStart:]))
		}

		parts[i].Text = retained.String()
		textOffset = end
	}

	var flattened strings.Builder
	for _, part := range parts {
		if part.Type == ToolContentPartText {
			flattened.WriteString(part.Text)
		}
	}
	return flattened.String(), parts
}

func truncateToolResult(result ToolResult, maxChars int) ToolResult {
	result.Content, result.ContentParts = truncateToolResultContent(result.Content, result.ContentParts, maxChars)
	return result
}

func toolResultImageData(part ToolContentPart) (mediaType, base64Data string, ok bool) {
	if part.Type != ToolContentPartImageData || part.ImageData == nil {
		return "", "", false
	}

	mediaType = strings.TrimSpace(part.ImageData.MediaType)
	base64Data = strings.TrimSpace(part.ImageData.Base64)
	if !isSupportedToolResultImageMediaType(mediaType) || base64Data == "" {
		return "", "", false
	}
	if _, err := base64.StdEncoding.DecodeString(base64Data); err != nil {
		return "", "", false
	}
	return mediaType, base64Data, true
}

func toolResultHasImageData(result *ToolResult) bool {
	for _, part := range toolResultContentParts(result) {
		if _, _, ok := toolResultImageData(part); ok {
			return true
		}
	}
	return false
}

func isSupportedToolResultImageMediaType(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func imageDetail(detail string) string {
	switch strings.TrimSpace(strings.ToLower(detail)) {
	case "low", "high", "auto":
		return strings.TrimSpace(strings.ToLower(detail))
	default:
		return ""
	}
}

func imageDetailWithDefault(detail, defaultValue string) string {
	if normalized := imageDetail(detail); normalized != "" {
		return normalized
	}
	return defaultValue
}

// toolResultResponsesImageParts extracts image parts from a tool result
// and returns Responses API content parts suitable for injection as a
// synthetic user message. Only image parts are returned — text is already
// sent in the function_call_output and should not be duplicated.
// Returns nil if no image data is present.
func toolResultResponsesImageParts(result *ToolResult) (parts []ResponsesContentPart, hasImage bool) {
	for _, contentPart := range toolResultContentParts(result) {
		if contentPart.Type != ToolContentPartImageData {
			continue
		}
		mimeType, base64Data, ok := toolResultImageData(contentPart)
		if !ok {
			continue
		}
		hasImage = true
		dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64Data)
		parts = append(parts, ResponsesContentPart{Type: "input_image", ImageURL: dataURL, Detail: imageDetail(contentPart.ImageData.Detail)})
	}
	return parts, hasImage
}
