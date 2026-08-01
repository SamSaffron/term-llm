package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

type responsesRequest struct {
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Instructions      *string           `json:"instructions"`
	Metadata          map[string]string `json:"metadata"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	Reasoning         json.RawMessage   `json:"reasoning,omitempty"`
	Include           []string          `json:"include,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	ServiceTier       *string           `json:"service_tier,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
}

type responsesWireError struct {
	Status  int
	Code    string
	Message string
	Param   string
}

func (e *responsesWireError) Error() string {
	if e == nil {
		return "invalid Responses request"
	}
	return e.Message
}

func (s *Server) decodeResponsesRequest(r *http.Request) (responsesRequest, *responsesWireError) {
	data, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBodyBytes+1))
	if err != nil {
		return responsesRequest{}, invalidResponsesRequest("invalid_json", "could not read request body", "")
	}
	if int64(len(data)) > s.cfg.MaxBodyBytes {
		return responsesRequest{}, &responsesWireError{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Message: "request body exceeds the configured gateway limit"}
	}
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&fields); err != nil {
		return responsesRequest{}, invalidResponsesRequest("invalid_json", "request body must be valid JSON", "")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return responsesRequest{}, invalidResponsesRequest("invalid_json", "request body must contain one JSON object", "")
	}
	if fields == nil {
		return responsesRequest{}, invalidResponsesRequest("invalid_json", "request body must be a JSON object", "")
	}

	known := map[string]bool{
		"model": true, "input": true, "instructions": true, "max_output_tokens": true,
		"stream": true, "reasoning": true, "include": true, "temperature": true,
		"top_p": true, "service_tier": true, "parallel_tool_calls": true, "tools": true,
		"tool_choice": true, "metadata": true, "user": true, "store": true,
		"stream_options": true, "prompt_cache_key": true, "prompt_cache_retention": true,
		"safety_identifier": true, "background": true, "previous_response_id": true,
		"conversation": true, "text": true, "truncation": true, "max_tool_calls": true,
		"top_logprobs": true,
	}
	for name := range fields {
		if !known[name] {
			return responsesRequest{}, invalidResponsesRequest("unsupported_field", fmt.Sprintf("field %q is not supported by this Responses edge", name), name)
		}
	}
	if raw := fields["background"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return responsesRequest{}, invalidResponsesRequest("unsupported_field", "background responses are not supported", "background")
	}
	for _, name := range []string{"previous_response_id", "conversation", "text", "max_tool_calls"} {
		if raw := bytes.TrimSpace(fields[name]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte(`""`)) && !bytes.Equal(raw, []byte("{}")) {
			return responsesRequest{}, invalidResponsesRequest("unsupported_field", fmt.Sprintf("field %q is not supported; send complete stateless input instead", name), name)
		}
	}
	if raw := fields["truncation"]; len(raw) > 0 {
		var value string
		if json.Unmarshal(raw, &value) != nil || (value != "" && value != "disabled") {
			return responsesRequest{}, invalidResponsesRequest("unsupported_field", "only truncation=disabled is supported", "truncation")
		}
	}
	if raw := fields["top_logprobs"]; len(raw) > 0 {
		var value int
		if json.Unmarshal(raw, &value) != nil || value != 0 {
			return responsesRequest{}, invalidResponsesRequest("unsupported_field", "top_logprobs is not supported", "top_logprobs")
		}
	}

	var request responsesRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return responsesRequest{}, invalidResponsesRequest("invalid_json", "request fields have invalid types", "")
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" {
		return responsesRequest{}, invalidResponsesRequest("invalid_model_namespace", "model must use provider/model namespace", "model")
	}
	if len(bytes.TrimSpace(request.Input)) == 0 || bytes.Equal(bytes.TrimSpace(request.Input), []byte("null")) {
		return responsesRequest{}, invalidResponsesRequest("invalid_input", "input is required", "input")
	}
	if request.MaxOutputTokens != nil && *request.MaxOutputTokens <= 0 {
		return responsesRequest{}, invalidResponsesRequest("invalid_request", "max_output_tokens must be positive", "max_output_tokens")
	}
	if err := validateResponsesFloat(request.Temperature, "temperature", 0, 2); err != nil {
		return responsesRequest{}, err
	}
	if err := validateResponsesFloat(request.TopP, "top_p", 0, 1); err != nil {
		return responsesRequest{}, err
	}
	if request.ServiceTier != nil {
		switch strings.TrimSpace(*request.ServiceTier) {
		case "", "auto", "default", "flex", "scale", "priority":
		default:
			return responsesRequest{}, invalidResponsesRequest("invalid_service_tier", "service_tier must be auto, default, flex, scale, or priority", "service_tier")
		}
	}
	for _, include := range request.Include {
		if include != "reasoning.encrypted_content" {
			return responsesRequest{}, invalidResponsesRequest("unsupported_include", fmt.Sprintf("include value %q is not supported", include), "include")
		}
	}
	return request, nil
}

func validateResponsesFloat(value *float64, name string, minValue, maxValue float64) *responsesWireError {
	if value == nil {
		return nil
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < minValue || *value > maxValue {
		return invalidResponsesRequest("invalid_request", fmt.Sprintf("%s must be between %g and %g", name, minValue, maxValue), name)
	}
	return nil
}

func invalidResponsesRequest(code, message, param string) *responsesWireError {
	return &responsesWireError{Status: http.StatusBadRequest, Code: code, Message: message, Param: param}
}

func splitResponsesModel(value string) (string, string, *responsesWireError) {
	provider, model, found := strings.Cut(strings.TrimSpace(value), "/")
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if !found || provider == "" || model == "" {
		return "", "", invalidResponsesRequest("invalid_model_namespace", "model must use provider/model namespace with non-empty provider and model", "model")
	}
	return provider, model, nil
}

func translateResponsesRequest(request responsesRequest) (llm.Request, *responsesWireError) {
	messages, inputErr := decodeResponsesInput(request.Input)
	if inputErr != nil {
		return llm.Request{}, inputErr
	}
	if request.Instructions != nil && strings.TrimSpace(*request.Instructions) != "" {
		messages = append([]llm.Message{{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: *request.Instructions}}}}, messages...)
	}
	tools, toolErr := decodeResponsesTools(request.Tools)
	if toolErr != nil {
		return llm.Request{}, toolErr
	}
	choice, choiceErr := decodeResponsesToolChoice(request.ToolChoice, tools)
	if choiceErr != nil {
		return llm.Request{}, choiceErr
	}
	effort, reasoningErr := decodeResponsesReasoning(request.Reasoning)
	if reasoningErr != nil {
		return llm.Request{}, reasoningErr
	}
	out := llm.Request{
		Messages: messages, Tools: tools, ToolChoice: choice, ReasoningEffort: effort,
		DisableExternalWebFetch: true, ParallelToolCalls: true,
	}
	if request.ParallelToolCalls != nil {
		out.ParallelToolCalls = *request.ParallelToolCalls
	}
	if request.MaxOutputTokens != nil {
		out.MaxOutputTokens = *request.MaxOutputTokens
	}
	if request.Temperature != nil {
		out.Temperature = float32(*request.Temperature)
		out.TemperatureSet = true
	}
	if request.TopP != nil {
		out.TopP = float32(*request.TopP)
		out.TopPSet = true
	}
	if request.ServiceTier != nil {
		out.ServiceTier = strings.TrimSpace(*request.ServiceTier)
		out.ServiceTierSet = true
	}
	return out, nil
}

func decodeResponsesReasoning(raw json.RawMessage) (string, *responsesWireError) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", invalidResponsesRequest("invalid_reasoning", "reasoning must be an object", "reasoning")
	}
	for name := range fields {
		if name != "summary" && name != "effort" {
			return "", invalidResponsesRequest("unsupported_field", fmt.Sprintf("reasoning.%s is not supported", name), "reasoning."+name)
		}
	}
	var value struct {
		Summary string `json:"summary"`
		Effort  string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", invalidResponsesRequest("invalid_reasoning", "reasoning fields have invalid types", "reasoning")
	}
	if value.Summary != "" && value.Summary != "auto" {
		return "", invalidResponsesRequest("unsupported_reasoning_summary", "only reasoning.summary=auto is supported", "reasoning.summary")
	}
	return strings.TrimSpace(value.Effort), nil
}

func decodeResponsesTools(rawTools []json.RawMessage) ([]llm.ToolSpec, *responsesWireError) {
	tools := make([]llm.ToolSpec, 0, len(rawTools))
	seen := make(map[string]bool)
	for i, raw := range rawTools {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, invalidResponsesRequest("invalid_tool", fmt.Sprintf("tools[%d] must be an object", i), "tools")
		}
		var tool struct {
			Type        string                 `json:"type"`
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
			Strict      bool                   `json:"strict"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil || tool.Type != "function" {
			return nil, invalidResponsesRequest("unsupported_tool_type", "only flat Responses function tools are supported; gateway-side hosted/search tools are disabled", fmt.Sprintf("tools[%d].type", i))
		}
		for name := range fields {
			if name != "type" && name != "name" && name != "description" && name != "parameters" && name != "strict" {
				return nil, invalidResponsesRequest("unsupported_tool_field", fmt.Sprintf("tools[%d].%s is not supported", i, name), "tools")
			}
		}
		tool.Name = strings.TrimSpace(tool.Name)
		if tool.Name == "" || tool.Parameters == nil || seen[tool.Name] {
			return nil, invalidResponsesRequest("invalid_tool", "function tools require unique non-empty names and parameters", "tools")
		}
		seen[tool.Name] = true
		tools = append(tools, llm.ToolSpec{Name: tool.Name, Description: tool.Description, Schema: tool.Parameters, Strict: tool.Strict})
	}
	return tools, nil
}

func decodeResponsesToolChoice(raw json.RawMessage, tools []llm.ToolSpec) (llm.ToolChoice, *responsesWireError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return llm.ToolChoice{}, nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "none":
			return llm.ToolChoice{Mode: llm.ToolChoiceNone}, nil
		case "auto":
			return llm.ToolChoice{Mode: llm.ToolChoiceAuto}, nil
		case "required":
			return llm.ToolChoice{Mode: llm.ToolChoiceRequired}, nil
		default:
			return llm.ToolChoice{}, invalidResponsesRequest("invalid_tool_choice", "tool_choice must be none, auto, required, or a named function", "tool_choice")
		}
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &named); err != nil || named.Type != "function" || strings.TrimSpace(named.Name) == "" {
		return llm.ToolChoice{}, invalidResponsesRequest("invalid_tool_choice", "named tool_choice must be {type:function,name:...}", "tool_choice")
	}
	for _, tool := range tools {
		if tool.Name == named.Name {
			return llm.ToolChoice{Mode: llm.ToolChoiceName, Name: named.Name}, nil
		}
	}
	return llm.ToolChoice{}, invalidResponsesRequest("invalid_tool_choice", "named tool_choice does not match a supplied function tool", "tool_choice")
}

func decodeResponsesInput(raw json.RawMessage) ([]llm.Message, *responsesWireError) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: text}}}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, invalidResponsesRequest("invalid_input", "input must be a string or an array of Responses input items", "input")
	}
	messages := make([]llm.Message, 0, len(items))
	for i, rawItem := range items {
		message, err := decodeResponsesInputItem(rawItem, i)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func decodeResponsesInputItem(raw json.RawMessage, index int) (llm.Message, *responsesWireError) {
	var header struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return llm.Message{}, invalidResponsesRequest("invalid_input", fmt.Sprintf("input[%d] must be an object", index), "input")
	}
	if header.Type == "" && header.Role != "" {
		header.Type = "message"
	}
	switch header.Type {
	case "message":
		return decodeResponsesMessage(raw, index, header.Role)
	case "reasoning":
		return decodeResponsesReasoningItem(raw, index)
	case "function_call":
		return decodeResponsesFunctionCall(raw, index)
	case "function_call_output":
		return decodeResponsesFunctionOutput(raw, index)
	default:
		return llm.Message{}, invalidResponsesRequest("unsupported_input_type", fmt.Sprintf("input[%d] type %q is not supported", index, header.Type), "input")
	}
}

func decodeResponsesMessage(raw json.RawMessage, index int, roleName string) (llm.Message, *responsesWireError) {
	role, ok := mapResponsesRole(roleName)
	if !ok {
		return llm.Message{}, invalidResponsesRequest("invalid_role", fmt.Sprintf("input[%d] role must be developer, user, or assistant", index), "input")
	}
	var item struct {
		ID      string          `json:"id,omitempty"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || len(item.Content) == 0 {
		return llm.Message{}, invalidResponsesRequest("invalid_content", fmt.Sprintf("input[%d] message content is required", index), "input")
	}
	parts, err := decodeResponsesContent(item.Content, role, index)
	if err != nil {
		return llm.Message{}, err
	}
	message := llm.Message{Role: role, Parts: parts}
	if role == llm.RoleAssistant && strings.TrimSpace(item.ID) != "" {
		sanitized, marshalErr := json.Marshal(map[string]any{"type": "message", "id": strings.TrimSpace(item.ID), "role": "assistant", "content": responsesReplayContent(parts)})
		if marshalErr == nil {
			message.Parts = append([]llm.Part{{Type: llm.PartProviderReplay, ProviderReplay: &llm.ProviderReplayItem{Raw: sanitized}}}, message.Parts...)
		}
	}
	return message, nil
}

func mapResponsesRole(value string) (llm.Role, bool) {
	switch value {
	case "developer":
		return llm.RoleDeveloper, true
	case "user":
		return llm.RoleUser, true
	case "assistant":
		return llm.RoleAssistant, true
	default:
		return "", false
	}
}

func decodeResponsesContent(raw json.RawMessage, role llm.Role, itemIndex int) ([]llm.Part, *responsesWireError) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Part{{Type: llm.PartText, Text: text}}, nil
	}
	var content []struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		ImageURL string `json:"image_url,omitempty"`
		Detail   string `json:"detail,omitempty"`
		Filename string `json:"filename,omitempty"`
		FileData string `json:"file_data,omitempty"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, invalidResponsesRequest("invalid_content", fmt.Sprintf("input[%d].content must be a string or array", itemIndex), "input")
	}
	parts := make([]llm.Part, 0, len(content))
	for partIndex, part := range content {
		switch part.Type {
		case "input_text", "output_text", "text":
			parts = append(parts, llm.Part{Type: llm.PartText, Text: part.Text})
		case "input_image":
			if role == llm.RoleAssistant {
				return nil, invalidResponsesRequest("invalid_content", "assistant input_image content is not supported", "input")
			}
			mediaType, encoded, size, err := decodeResponsesDataURL(part.ImageURL)
			if err != nil || !strings.HasPrefix(mediaType, "image/") {
				return nil, invalidResponsesRequest("invalid_image", fmt.Sprintf("input[%d].content[%d] must contain a base64 image data URL", itemIndex, partIndex), "input")
			}
			_ = size
			parts = append(parts, llm.Part{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: mediaType, Base64: encoded, Detail: part.Detail}})
		case "input_file":
			if role == llm.RoleAssistant {
				return nil, invalidResponsesRequest("invalid_content", "assistant input_file content is not supported", "input")
			}
			mediaType, encoded, size, err := decodeResponsesDataURL(part.FileData)
			if err != nil {
				return nil, invalidResponsesRequest("invalid_file", fmt.Sprintf("input[%d].content[%d] must contain a base64 file data URL", itemIndex, partIndex), "input")
			}
			parts = append(parts, llm.Part{Type: llm.PartFile, FileData: &llm.ToolFileData{MediaType: mediaType, Base64: encoded, Filename: part.Filename, SizeBytes: int64(size)}})
		default:
			return nil, invalidResponsesRequest("unsupported_content_type", fmt.Sprintf("input[%d].content[%d] type %q is not supported", itemIndex, partIndex, part.Type), "input")
		}
	}
	return parts, nil
}

func decodeResponsesDataURL(value string) (string, string, int, error) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", 0, fmt.Errorf("not a data URL")
	}
	header, payload, found := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
	if !found || !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return "", "", 0, fmt.Errorf("not base64")
	}
	mediaType := strings.TrimSpace(strings.TrimSuffix(header, ";base64"))
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = parsed
	} else {
		return "", "", 0, err
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", "", 0, err
	}
	return mediaType, payload, len(decoded), nil
}

func decodeResponsesReasoningItem(raw json.RawMessage, index int) (llm.Message, *responsesWireError) {
	var item struct {
		ID               string `json:"id"`
		EncryptedContent string `json:"encrypted_content"`
		Summary          []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || strings.TrimSpace(item.ID) == "" {
		return llm.Message{}, invalidResponsesRequest("invalid_reasoning", fmt.Sprintf("input[%d] reasoning item requires a non-empty id", index), "input")
	}
	summaries := make([]string, 0, len(item.Summary))
	for _, summary := range item.Summary {
		if summary.Type != "summary_text" {
			return llm.Message{}, invalidResponsesRequest("invalid_reasoning", "reasoning summary parts must use type summary_text", "input")
		}
		summaries = append(summaries, summary.Text)
	}
	part := llm.Part{Type: llm.PartText, ReasoningItemID: strings.TrimSpace(item.ID), ReasoningEncryptedContent: item.EncryptedContent, ReasoningSummaryParts: summaries, ReasoningContent: strings.Join(summaries, "\n\n"), ReasoningKind: llm.ReasoningKindSummary}
	if len(summaries) == 0 && item.EncryptedContent != "" {
		part.ReasoningKind = llm.ReasoningKindEncrypted
	}
	sanitized, _ := json.Marshal(map[string]any{"type": "reasoning", "id": strings.TrimSpace(item.ID), "encrypted_content": item.EncryptedContent, "summary": item.Summary})
	return llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartProviderReplay, ProviderReplay: &llm.ProviderReplayItem{Raw: sanitized}}, part}}, nil
}

func decodeResponsesFunctionCall(raw json.RawMessage, index int) (llm.Message, *responsesWireError) {
	var item struct {
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || strings.TrimSpace(item.CallID) == "" || strings.TrimSpace(item.Name) == "" || !json.Valid([]byte(item.Arguments)) {
		return llm.Message{}, invalidResponsesRequest("invalid_function_call", fmt.Sprintf("input[%d] function_call requires call_id, name, and JSON arguments", index), "input")
	}
	replay := map[string]any{"type": "function_call", "call_id": item.CallID, "name": item.Name, "arguments": item.Arguments}
	if itemID := strings.TrimSpace(item.ID); itemID != "" {
		replay["id"] = itemID
	}
	sanitized, _ := json.Marshal(replay)
	call := &llm.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}
	return llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartProviderReplay, ProviderReplay: &llm.ProviderReplayItem{Raw: sanitized}}, {Type: llm.PartToolCall, ToolCall: call}}}, nil
}

func decodeResponsesFunctionOutput(raw json.RawMessage, index int) (llm.Message, *responsesWireError) {
	var item struct {
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || strings.TrimSpace(item.CallID) == "" {
		return llm.Message{}, invalidResponsesRequest("invalid_function_call_output", fmt.Sprintf("input[%d] function_call_output requires call_id and string output", index), "input")
	}
	return llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: item.CallID, Content: item.Output}}}}, nil
}

func responsesReplayContent(parts []llm.Part) []map[string]any {
	content := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		if part.Type == llm.PartText {
			content = append(content, map[string]any{"type": "output_text", "text": part.Text})
		}
	}
	return content
}
