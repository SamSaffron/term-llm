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
		detail := normalizeVisionDetail(contentPart.ImageData.Detail)
		parts = append(parts, ResponsesContentPart{Type: "input_image", ImageURL: dataURL, Detail: detail})
	}
	return parts, hasImage
}
