package responses

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

// WebSearchToolName mirrors llm.WebSearchToolName.
const WebSearchToolName = "web_search"

var (
	sseEventField = stream.EventField
	sseDataField  = stream.DataField
	sseDoneData   = stream.DoneData
)

type eventSender = stream.Sender

func newEventStream(ctx context.Context, run func(context.Context, eventSender) error) Stream {
	return stream.New(ctx, run)
}

func newEventStreamWithCancelHook(ctx context.Context, cancelHook func(), run func(context.Context, eventSender) error) Stream {
	return stream.NewWithCancelHook(ctx, cancelHook, run)
}

func sanitizeToolHistory(messages []Message) []Message { return core.SanitizeToolHistory(messages) }

func collectTextParts(parts []Part) string { return core.CollectTextParts(parts) }

func toolResultTextContent(result *core.ToolResult) string { return core.ToolResultTextContent(result) }

func imageDetail(detail string) string { return core.ImageDetail(detail) }

func readSSELineBytes(reader *bufio.Reader) ([]byte, bool, error) {
	return stream.ReadLineBytes(reader)
}

func boolPtr(v bool) *bool { return &v }

func newHTTPStatusErrorMessagef(resp *http.Response, body []byte, format string, args ...any) *providerhttp.StatusError {
	return providerhttp.NewStatusErrorMessage(fmt.Sprintf(format, args...), resp, body)
}

// newSyntheticToolCallID returns an opaque internal ID for a tool invocation
// whose provider protocol did not supply one.
func newSyntheticToolCallID() string { return "call_" + rand.Text() }

// toolResultResponsesImageParts extracts image parts from a tool result and
// returns Responses API content parts suitable for injection as a synthetic
// user message. Text is already sent in function_call_output.
func toolResultResponsesImageParts(result *core.ToolResult) (parts []ResponsesContentPart, hasImage bool) {
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
		parts = append(parts, ResponsesContentPart{Type: "input_image", ImageURL: dataURL, Detail: imageDetail(contentPart.ImageData.Detail)})
	}
	return parts, hasImage
}

func mimeSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func toolFileSizeBytes(file *core.ToolFileData) int64 {
	if file == nil {
		return 0
	}
	if file.SizeBytes > 0 {
		return file.SizeBytes
	}
	return decodedBase64Size(file.Base64)
}

func decodedBase64Size(base64Data string) int64 {
	base64Data = strings.NewReplacer("\r", "", "\n", "").Replace(base64Data)
	if base64Data == "" {
		return 0
	}
	decodedLen := base64.StdEncoding.DecodedLen(len(base64Data))
	if strings.HasSuffix(base64Data, "=") {
		decodedLen--
	}
	if strings.HasSuffix(base64Data, "==") {
		decodedLen--
	}
	if decodedLen < 0 {
		return 0
	}
	return int64(decodedLen)
}

// normalizeMediaType lowercases a MIME type and strips optional parameters.
func normalizeMediaType(mediaType string) string {
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))
	if mediaType == "" {
		return ""
	}
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = parsed
	} else if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = strings.TrimSpace(mediaType[:idx])
	}
	if mediaType == "image/jpg" {
		return "image/jpeg"
	}
	return mediaType
}

func mimeAllowed(mediaType string, allowed []string) bool {
	mediaType = normalizeMediaType(mediaType)
	if mediaType == "" {
		return false
	}
	for _, candidate := range allowed {
		candidate = strings.TrimSpace(strings.ToLower(candidate))
		if candidate == "" {
			continue
		}
		if candidate == mediaType || candidate == "*" || candidate == "*/*" {
			return true
		}
		if strings.HasSuffix(candidate, "/*") {
			if strings.HasPrefix(mediaType, strings.TrimSuffix(candidate, "*")) {
				return true
			}
			continue
		}
		if normalizeMediaType(candidate) == mediaType {
			return true
		}
	}
	return false
}

func debugRawHTTPErrorResponse(enabled bool, label string, resp *http.Response, body []byte) {
	if !enabled {
		return
	}
	var debugInfo strings.Builder
	fmt.Fprintf(&debugInfo, "Status: %d %s\n", resp.StatusCode, resp.Status)
	debugInfo.WriteString("Headers:\n")
	for key, values := range resp.Header {
		for _, value := range values {
			fmt.Fprintf(&debugInfo, "  %s: %s\n", key, value)
		}
	}
	debugInfo.WriteString("Body:\n")
	var prettyBody bytes.Buffer
	if json.Indent(&prettyBody, body, "", "  ") == nil {
		debugInfo.WriteString(prettyBody.String())
	} else {
		debugInfo.Write(body)
	}
	debugRawSection(enabled, label, debugInfo.String())
}
