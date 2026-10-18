package llm

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
)

// toolResultResponsesImageParts extracts image parts from a tool result
// and returns Responses API content parts suitable for injection as a
// synthetic user message. Only image parts are returned — text is already
// sent in the function_call_output and should not be duplicated.
// Returns nil if no image data is present.
func toolResultResponsesImageParts(result *core.ToolResult) (parts []responses.ResponsesContentPart, hasImage bool) {
	for _, contentPart := range core.ToolResultContentParts(result) {
		if contentPart.Type != core.ToolContentPartImageData {
			continue
		}
		mimeType, base64Data, ok := core.ToolResultImageData(contentPart)
		if !ok {
			continue
		}
		hasImage = true
		dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64Data)
		parts = append(parts, responses.ResponsesContentPart{Type: "input_image", ImageURL: dataURL, Detail: core.ImageDetail(contentPart.ImageData.Detail)})
	}
	return parts, hasImage
}
