package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"strings"
)

func BuildResponsesInputWithInstructions(messages []Message) (instructions string, input []ResponsesInputItem) {
	return BuildResponsesInputWithInstructionsAndFilePolicy(messages, nil)
}

// BuildResponsesInputWithInstructionsAndFilePolicy is like
// BuildResponsesInputWithInstructions but gates native input_file parts using
// the supplied provider policy.
func BuildResponsesInputWithInstructionsAndFilePolicy(messages []Message, policy FilePolicy) (instructions string, input []ResponsesInputItem) {
	messages = sanitizeToolHistory(messages)
	var systemParts []string
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			if text := collectTextParts(msg.Parts); text != "" {
				systemParts = append(systemParts, text)
			}
		default:
			input = append(input, buildResponsesInputForRole(msg, policy)...)
		}
	}
	return strings.Join(systemParts, "\n\n"), sanitizeResponsesToolItems(input)
}

// sanitizeResponsesToolItems is a final integrity check for stateless Responses
// input. Opaque provider replay can contain function calls that the generic
// message sanitizer repaired or removed, so pair calls and outputs again after
// replay items have been expanded.
func sanitizeResponsesToolItems(items []ResponsesInputItem) []ResponsesInputItem {
	pending := make(map[string][]int)
	matched := make([]bool, len(items))
	toolItem := make([]bool, len(items))

	for i, item := range items {
		itemType, callID := responsesToolItemRef(item)
		switch itemType {
		case "function_call":
			toolItem[i] = true
			if callID != "" {
				pending[callID] = append(pending[callID], i)
			}
		case "function_call_output":
			toolItem[i] = true
			calls := pending[callID]
			if callID == "" || len(calls) == 0 {
				continue
			}
			callIndex := calls[0]
			if len(calls) == 1 {
				delete(pending, callID)
			} else {
				pending[callID] = calls[1:]
			}
			matched[callIndex] = true
			matched[i] = true
		}
	}

	valid := items[:0]
	for i, item := range items {
		if !toolItem[i] || matched[i] {
			valid = append(valid, item)
		}
	}
	return valid
}

func responsesToolItemRef(item ResponsesInputItem) (itemType, callID string) {
	if len(item.Raw) == 0 {
		return strings.TrimSpace(item.Type), strings.TrimSpace(item.CallID)
	}
	if !bytes.Contains(item.Raw, []byte(`"function_call`)) {
		return "", ""
	}
	var ref struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(item.Raw, &ref); err != nil {
		return "", ""
	}
	return strings.TrimSpace(ref.Type), strings.TrimSpace(ref.CallID)
}

func buildResponsesInputItems(messages []Message, policy FilePolicy) []ResponsesInputItem {
	var inputItems []ResponsesInputItem
	for _, msg := range messages {
		if msg.Role == RoleSystem || msg.Role == RoleDeveloper {
			inputItems = append(inputItems, buildResponsesMessageItems("developer", msg.Parts, policy)...)
		} else {
			inputItems = append(inputItems, buildResponsesInputForRole(msg, policy)...)
		}
	}
	return inputItems
}

// BuildResponsesInput converts []Message to Open Responses input format.
func BuildResponsesInput(messages []Message) []ResponsesInputItem {
	return BuildResponsesInputWithFilePolicy(messages, nil)
}

// BuildResponsesInputWithFilePolicy converts []Message to Open Responses input
// format and sends PartFile as native input_file only when policy allows its
// MIME type and decoded size. Passing nil uses OpenAI Responses defaults.
func BuildResponsesInputWithFilePolicy(messages []Message, policy FilePolicy) []ResponsesInputItem {
	return sanitizeResponsesToolItems(buildResponsesInputItems(sanitizeToolHistory(messages), policy))
}

// BuildResponsesContinuationInput converts only the newest turn payload needed for a
// server-state continuation. Unlike BuildResponsesInput it intentionally skips
// whole-transcript tool-history sanitization so trailing tool results can be sent
// back against server-side conversation state without rebuilding earlier turns.
func BuildResponsesContinuationInput(messages []Message) []ResponsesInputItem {
	return BuildResponsesContinuationInputWithFilePolicy(messages, nil)
}

// BuildResponsesContinuationInputWithFilePolicy converts only the newest turn
// payload needed for a server-state continuation, using the supplied file policy
// for any new file parts.
func BuildResponsesContinuationInputWithFilePolicy(messages []Message, policy FilePolicy) []ResponsesInputItem {
	return buildResponsesContinuationInputWithFilePolicy(messages, policy, false)
}

func buildResponsesContinuationInputWithFilePolicy(messages []Message, policy FilePolicy, includeDeveloper bool) []ResponsesInputItem {
	messages = core.FilterConversationMessages(messages)
	if len(messages) == 0 {
		return nil
	}

	start := len(messages)
	sawToolResult := false
	for start > 0 {
		msg := messages[start-1]
		if msg.Role == RoleUser && !sawToolResult {
			start--
			continue
		}
		if msg.Role == RoleTool {
			sawToolResult = true
			start--
			continue
		}
		break
	}
	if !sawToolResult {
		start = 0
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == RoleUser {
				start = i
				if includeDeveloper {
					for start > 0 && messages[start-1].Role == RoleDeveloper {
						start--
					}
				}
				break
			}
		}
	}

	return buildResponsesInputItems(messages[start:], policy)
}

// buildResponsesInputForRole converts a single non-system message to input items.
func buildResponsesInputForRole(msg Message, policy FilePolicy) []ResponsesInputItem {
	switch msg.Role {
	case RoleUser:
		return buildResponsesMessageItems("user", msg.Parts, policy)
	case RoleDeveloper:
		return buildResponsesMessageItems("developer", msg.Parts, policy)
	case RoleAssistant:
		return buildResponsesAssistantItems(msg.Parts)
	case RoleTool:
		var items []ResponsesInputItem
		for _, part := range msg.Parts {
			if part.Type != PartToolResult || part.ToolResult == nil {
				continue
			}
			callID := strings.TrimSpace(part.ToolResult.ID)
			if callID == "" {
				continue
			}
			items = append(items, ResponsesInputItem{
				Type:   "function_call_output",
				CallID: callID,
				Output: toolResultTextContent(part.ToolResult),
				Caller: part.ToolResult.Caller,
			})
			if richParts, hasImage := toolResultResponsesImageParts(part.ToolResult); hasImage {
				items = append(items, ResponsesInputItem{
					Type:    "message",
					Role:    "user",
					Content: richParts,
				})
			}
		}
		return items
	default:
		return nil
	}
}

func buildResponsesMessageItems(role string, parts []Part, policy FilePolicy) []ResponsesInputItem {
	var items []ResponsesInputItem
	var textBuf strings.Builder
	flushText := func() {
		if textBuf.Len() == 0 {
			return
		}
		items = append(items, ResponsesInputItem{Type: "message", Role: role, Content: textBuf.String()})
		textBuf.Reset()
	}

	for _, part := range parts {
		switch part.Type {
		case PartProviderReplay:
			flushText()
			if part.ProviderReplay != nil && len(part.ProviderReplay.Raw) > 0 {
				items = append(items, ResponsesInputItem{Raw: append(json.RawMessage(nil), part.ProviderReplay.Raw...)})
			}
		case PartText:
			if part.Text != "" {
				textBuf.WriteString(part.Text)
			}
		case PartImage:
			if part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
				flushText()
				dataURL := fmt.Sprintf("data:%s;base64,%s", part.ImageData.MediaType, part.ImageData.Base64)
				imageParts := []ResponsesContentPart{{Type: "input_image", ImageURL: dataURL, Detail: imageDetail(part.ImageData.Detail)}}
				if part.ImagePath != "" {
					imageParts = append(imageParts, ResponsesContentPart{Type: "input_text", Text: "[image saved at: " + part.ImagePath + "]"})
				}
				items = append(items, ResponsesInputItem{Type: "message", Role: role, Content: imageParts})
			}
		case PartFile:
			switch {
			case responsesFilePrefersTextEmbed(part, policy):
				// The stored part already carries the file body fenced as
				// markdown (see FormatEmbeddedFileText), so inlining it cannot be
				// rejected by the provider's MIME allowlist. Native input_file
				// stays reserved for binary documents and spreadsheets.
				textBuf.WriteString(part.Text)
			case responseNativeFileAllowed(part, policy):
				flushText()
				items = append(items, ResponsesInputItem{Type: "message", Role: role, Content: []ResponsesContentPart{responsesNativeFilePart(part)}})
			default:
				if text := responseFileTextFallback(part, policy); text != "" {
					textBuf.WriteString(text)
				}
			}
		case PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			flushText()
			callID := strings.TrimSpace(part.ToolCall.ID)
			if callID == "" {
				continue
			}
			args := strings.TrimSpace(string(part.ToolCall.Arguments))
			if args == "" {
				args = "{}"
			}
			name := part.ToolCall.Name
			if part.ToolCall.ChildName != "" {
				name = part.ToolCall.ChildName
			}
			items = append(items, ResponsesInputItem{Type: "function_call", CallID: callID, Name: name, Namespace: part.ToolCall.Namespace, Arguments: args})
		}
	}
	flushText()
	return items
}

func effectiveResponsesFilePolicy(policy FilePolicy) FilePolicy {
	if isNilFilePolicy(policy) {
		def := defaultOpenAIResponsesFileUploadPolicy()
		return def
	}
	return policy
}

// responsesSpreadsheetMIMETypes are the upload types the builder keeps on the
// native input_file path even when their contents are text-like: the provider
// augments spreadsheets (formula ranges, table semantics) and a large CSV would
// otherwise be inlined verbatim into the prompt. The Excel and Google Sheets
// entries are not text-like to begin with; they are listed so the carve-out is
// documented in one place.
var responsesSpreadsheetMIMETypes = mimeSet([]string{
	"application/csv",
	"application/vnd.google-apps.spreadsheet",
	"application/vnd.ms-excel",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/x-iif",
	"text/csv",
	"text/tsv",
	"text/x-iif",
})

// maxPreferredTextEmbedBytes caps the upload size that is inlined in preference
// to a native input_file. Beyond it the provider's own extraction is the better
// deal: inlining a multi-megabyte log would blow the context window on every
// replay of that turn. Larger text uploads fall through to the native branch and
// only then to the notice, so nothing is silently dropped.
const maxPreferredTextEmbedBytes = 1 << 20

// responsesFileMediaType returns the provider-facing media type of an upload: the
// client/OS label canonicalized onto a token the provider recognizes, with the
// generic binary default when nothing is known. Validation and serialization must
// both go through this so a file can never be approved as one type and sent as
// another.
//
// Only an embedded body proves the bytes are text (ingest verifies UTF-8 with no
// NUL bytes before embedding), so only then may the filename extension override a
// binary-sounding label.
func responsesFileMediaType(part Part) string {
	if part.FileData == nil {
		return "application/octet-stream"
	}
	var mediaType string
	if isEmbeddedFileText(part.Text) {
		mediaType = canonicalTextUploadMediaType(part.FileData.Filename, part.FileData.MediaType)
	} else {
		mediaType = canonicalUploadMediaType(part.FileData.Filename, part.FileData.MediaType)
	}
	if mediaType == "" {
		return "application/octet-stream"
	}
	return mediaType
}

// responsesFilePrefersTextEmbed reports whether an upload should travel as plain
// text instead of a provider-native file. It requires an embedded body: a
// FormatUploadedFileNotice stub says only where the file lives, so treating it as
// inline content would drop the base64 the provider could have decoded itself.
func responsesFilePrefersTextEmbed(part Part, policy FilePolicy) bool {
	if part.FileData == nil || !isEmbeddedFileText(part.Text) {
		return false
	}
	sizeBytes := toolFileSizeBytes(part.FileData)
	if sizeBytes > maxPreferredTextEmbedBytes {
		return false
	}
	mediaType := responsesFileMediaType(part)
	if responsesSpreadsheetMIMETypes[mediaType] || !isTextLikeMediaType(mediaType) {
		return false
	}
	return effectiveResponsesFilePolicy(policy).AllowsTextEmbed(mediaType, sizeBytes)
}

// responsesNativeFilePart builds the input_file content part for an upload whose
// media type has already been validated by responseNativeFileAllowed.
func responsesNativeFilePart(part Part) ResponsesContentPart {
	filename := strings.TrimSpace(part.FileData.Filename)
	if filename == "" {
		filename = "upload"
	}
	return ResponsesContentPart{
		Type:     "input_file",
		Filename: filename,
		FileData: fmt.Sprintf("data:%s;base64,%s", responsesFileMediaType(part), part.FileData.Base64),
	}
}

func responseNativeFileAllowed(part Part, policy FilePolicy) bool {
	file := part.FileData
	if file == nil || strings.TrimSpace(file.Base64) == "" {
		return false
	}
	active := effectiveResponsesFilePolicy(policy)
	return active.AllowsNative(responsesFileMediaType(part), toolFileSizeBytes(file))
}

func responseFileTextFallback(part Part, policy FilePolicy) string {
	if part.Text == "" {
		return ""
	}
	if part.FileData == nil {
		return part.Text
	}
	active := effectiveResponsesFilePolicy(policy)
	if active.AllowsTextEmbed(responsesFileMediaType(part), toolFileSizeBytes(part.FileData)) {
		return part.Text
	}
	return formatUploadedFileNotice(part.FileData.Filename, part.FileData.MediaType, part.FilePath, toolFileSizeBytes(part.FileData))
}

func buildResponsesAssistantItems(parts []Part) []ResponsesInputItem {
	var replay []ResponsesInputItem
	for _, part := range parts {
		if part.Type == PartProviderReplay && part.ProviderReplay != nil && len(part.ProviderReplay.Raw) > 0 {
			replay = append(replay, ResponsesInputItem{Raw: append(json.RawMessage(nil), part.ProviderReplay.Raw...)})
		}
	}
	if len(replay) > 0 {
		return replay
	}

	var items []ResponsesInputItem
	var textBuf strings.Builder

	flushText := func() {
		if textBuf.Len() == 0 {
			return
		}
		items = append(items, ResponsesInputItem{
			Type:    "message",
			Role:    "assistant",
			Content: textBuf.String(),
		})
		textBuf.Reset()
	}

	for _, part := range parts {
		switch part.Type {
		case PartProviderReplay:
			flushText()
			if part.ProviderReplay != nil && len(part.ProviderReplay.Raw) > 0 {
				items = append(items, ResponsesInputItem{Raw: append(json.RawMessage(nil), part.ProviderReplay.Raw...)})
			}
		case PartText:
			if hasResponsesReasoningReplay(part) {
				flushText()
				items = append(items, buildResponsesReasoningItem(part))
			}
			if part.Text != "" {
				textBuf.WriteString(part.Text)
			}

		case PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			flushText()
			callID := strings.TrimSpace(part.ToolCall.ID)
			if callID == "" {
				continue
			}
			args := strings.TrimSpace(string(part.ToolCall.Arguments))
			if args == "" {
				args = "{}"
			}
			name := part.ToolCall.Name
			if part.ToolCall.ChildName != "" {
				name = part.ToolCall.ChildName
			}
			items = append(items, ResponsesInputItem{
				Type:      "function_call",
				CallID:    callID,
				Name:      name,
				Namespace: part.ToolCall.Namespace,
				Arguments: args,
			})
		}
	}

	flushText()
	return items
}

func hasResponsesReasoningReplay(part Part) bool {
	return strings.TrimSpace(part.ReasoningItemID) != "" || strings.TrimSpace(part.ReasoningEncryptedContent) != ""
}

func buildResponsesReasoningItem(part Part) ResponsesInputItem {
	summary := responsesReasoningSummary{}
	item := ResponsesInputItem{
		Type:             "reasoning",
		ID:               strings.TrimSpace(part.ReasoningItemID),
		EncryptedContent: strings.TrimSpace(part.ReasoningEncryptedContent),
		Summary:          &summary,
	}
	if part.ReasoningKind != ReasoningKindRaw {
		for _, text := range reasoningSummaryTexts(part) {
			summary = append(summary, responsesReasoningSummaryPart{
				Type: "summary_text",
				Text: text,
			})
		}
		item.Summary = &summary
	}
	return item
}

func reasoningSummaryTexts(part Part) []string {
	var out []string
	for _, text := range part.ReasoningSummaryParts {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) > 0 {
		return out
	}
	if text := strings.TrimSpace(part.ReasoningContent); text != "" {
		return []string{text}
	}
	return nil
}

// BuildResponsesTools converts []ToolSpec to Open Responses format with schema normalization.
func BuildResponsesTools(specs []ToolSpec) []any {
	return BuildResponsesToolsWithOptions(specs, ProgrammaticToolCallingOptions{})
}

// BuildResponsesToolsWithOptions applies PTC eligibility only to explicitly
// named tools and appends the hosted programmatic_tool_calling tool when enabled.
func BuildResponsesToolsWithOptions(specs []ToolSpec, ptc ProgrammaticToolCallingOptions) []any {
	if len(specs) == 0 && !ptc.Enabled {
		return nil
	}
	eligible := make(map[string]bool, len(ptc.Tools))
	for _, name := range ptc.Tools {
		if name = strings.TrimSpace(name); name != "" {
			eligible[name] = true
		}
	}
	tools := make([]any, 0, len(specs)+1)
	for _, spec := range specs {
		strict := spec.Strict
		params := openAIParametersFromToolSchema(spec.Schema, strict)
		allowed := append([]string(nil), spec.AllowedCallers...)
		if ptc.Enabled && eligible[spec.Name] && !containsResponseString(allowed, "programmatic") {
			allowed = append(allowed, "programmatic")
		}
		tools = append(tools, ResponsesTool{
			Type:           "function",
			Name:           spec.Name,
			Description:    spec.Description,
			Parameters:     params,
			Strict:         strict,
			AllowedCallers: allowed,
		})
	}
	if ptc.Enabled {
		tools = append(tools, map[string]any{"type": "programmatic_tool_calling"})
	}
	return tools
}

func containsResponseString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// BuildResponsesToolChoice converts ToolChoice to Open Responses format
func BuildResponsesToolChoice(choice ToolChoice) interface{} {
	switch choice.Mode {
	case ToolChoiceNone:
		return "none"
	case ToolChoiceRequired:
		return "required"
	case ToolChoiceAuto:
		return "auto"
	case ToolChoiceName:
		return map[string]interface{}{
			"type": "function",
			"name": choice.Name,
		}
	default:
		return nil
	}
}
