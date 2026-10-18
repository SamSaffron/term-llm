package llm

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func toolResultContentParts(result *ToolResult) []ToolContentPart {
	return core.ToolResultContentParts(result)
}

func toolResultTextContent(result *ToolResult) string { return core.ToolResultTextContent(result) }

func truncateToolOutput(output ToolOutput, maxChars int) ToolOutput {
	return core.TruncateToolOutput(output, maxChars)
}

func truncateToolResultContent(content string, parts []ToolContentPart, maxChars int) (string, []ToolContentPart) {
	return core.TruncateToolResultContent(content, parts, maxChars)
}

func truncateToolResult(result ToolResult, maxChars int) ToolResult {
	return core.TruncateToolResultValue(result, maxChars)
}

func toolResultImageData(part ToolContentPart) (mediaType, base64Data string, ok bool) {
	return core.ToolResultImageData(part)
}

func toolResultHasImageData(result *ToolResult) bool { return core.ToolResultHasImageData(result) }

func isSupportedToolResultImageMediaType(mimeType string) bool {
	return core.IsSupportedToolResultImageMediaType(mimeType)
}

func imageDetail(detail string) string { return core.ImageDetail(detail) }

func imageDetailWithDefault(detail, defaultValue string) string {
	return core.ImageDetailWithDefault(detail, defaultValue)
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
