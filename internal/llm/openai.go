package llm

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// OpenAIProvider implements Provider using the standard OpenAI API.
type OpenAIProvider struct {
	client          *openai.Client // Used for ListModels
	apiKey          string
	model           string
	effort          string           // reasoning effort: "low", "medium", "high", "xhigh", or ""
	responsesClient *ResponsesClient // Shared client for Responses API with server state
}

// parseModelEffort extracts effort suffix from model name.
// "gpt-5.2-high" -> ("gpt-5.2", "high")
// "gpt-5.2-xhigh" -> ("gpt-5.2", "xhigh")
// "gpt-5.2" -> ("gpt-5.2", "")
func parseModelEffort(model string) (string, string) {
	// Check suffixes in order from longest to shortest to avoid "-high" matching "-xhigh"
	suffixes := []string{"xhigh", "medium", "high", "low"}
	for _, effort := range suffixes {
		suffix := "-" + effort
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), effort
		}
	}
	return model, ""
}

func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	actualModel, effort := parseModelEffort(model)
	client := openai.NewClient(option.WithAPIKey(apiKey))
	return &OpenAIProvider{
		client: &client,
		apiKey: apiKey,
		model:  actualModel,
		effort: effort,
	}
}

func (p *OpenAIProvider) Name() string {
	if p.effort != "" {
		return fmt.Sprintf("OpenAI (%s, effort=%s)", p.model, p.effort)
	}
	return fmt.Sprintf("OpenAI (%s)", p.model)
}

func (p *OpenAIProvider) Credential() string {
	return "api_key"
}

func (p *OpenAIProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    true,
		NativeWebFetch:     false, // No native URL fetch
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	page, err := p.client.Models.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}

	var models []ModelInfo
	for _, m := range page.Data {
		models = append(models, ModelInfo{
			ID:         m.ID,
			Created:    m.Created,
			InputLimit: InputLimitForModel(m.ID),
		})
	}

	return models, nil
}

func (p *OpenAIProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	// Reuse client to maintain server state across requests
	if p.responsesClient == nil {
		p.responsesClient = &ResponsesClient{
			BaseURL:       "https://api.openai.com/v1/responses",
			GetAuthHeader: func() string { return "Bearer " + p.apiKey },
			HTTPClient:    defaultHTTPClient,
		}
	}

	// Strip effort suffix from req.Model if present, use it if no provider-level effort set
	reqModel, reqEffort := parseModelEffort(req.Model)
	model := chooseModel(reqModel, p.model)
	effort := p.effort
	if effort == "" && reqEffort != "" {
		effort = reqEffort
	}

	// Build tools - add web search tool first if requested
	tools := BuildResponsesTools(req.Tools)
	if req.Search {
		webSearchTool := ResponsesWebSearchTool{Type: "web_search_preview"}
		tools = append([]any{webSearchTool}, tools...)
	}

	responsesReq := ResponsesRequest{
		Model:          model,
		Input:          BuildResponsesInput(req.Messages),
		Tools:          tools,
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: req.SessionID,
		Stream:         true,
		SessionID:      req.SessionID,
	}

	if req.ToolChoice.Mode != "" {
		responsesReq.ToolChoice = BuildResponsesToolChoice(req.ToolChoice)
	}
	if req.ParallelToolCalls {
		responsesReq.ParallelToolCalls = boolPtr(true)
	}
	if req.Temperature > 0 {
		v := float64(req.Temperature)
		responsesReq.Temperature = &v
	}
	if req.TopP > 0 {
		v := float64(req.TopP)
		responsesReq.TopP = &v
	}
	if req.MaxOutputTokens > 0 {
		responsesReq.MaxOutputTokens = req.MaxOutputTokens
	}
	responsesReq.Reasoning = &ResponsesReasoning{Summary: "auto"}
	if effort != "" {
		responsesReq.Reasoning.Effort = effort
	}

	if req.Debug {
		systemPreview := collectRoleText(req.Messages, RoleSystem)
		userPreview := collectRoleText(req.Messages, RoleUser)
		fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI Stream Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Developer: %s\n", truncate(systemPreview, 200))
		fmt.Fprintf(os.Stderr, "User: %s\n", truncate(userPreview, 200))
		fmt.Fprintf(os.Stderr, "Input Items: %d\n", len(responsesReq.Input))
		fmt.Fprintf(os.Stderr, "Tools: %d\n", len(tools))
		fmt.Fprintln(os.Stderr, "===================================")
	}

	return p.responsesClient.Stream(ctx, responsesReq, req.DebugRaw)
}

// ResetConversation clears server state for the Responses API client.
// Called on /clear or new conversation.
func (p *OpenAIProvider) ResetConversation() {
	if p.responsesClient != nil {
		p.responsesClient.ResetConversation()
	}
}

// normalizeSchemaForOpenAI ensures schema meets OpenAI's strict requirements:
// - 'required' must include every key in properties
// - 'additionalProperties' must be false
// - unsupported 'format' values must be removed
func normalizeSchemaForOpenAI(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return schema
	}
	return normalizeSchemaRecursive(deepCopyMap(schema))
}

// deepCopyMap creates a deep copy of a map[string]interface{}
func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			result[k] = deepCopyMap(val)
		case []interface{}:
			result[k] = deepCopySlice(val)
		default:
			result[k] = v
		}
	}
	return result
}

func deepCopySlice(s []interface{}) []interface{} {
	if s == nil {
		return nil
	}
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]interface{}:
			result[i] = deepCopyMap(val)
		case []interface{}:
			result[i] = deepCopySlice(val)
		default:
			result[i] = v
		}
	}
	return result
}

// normalizeSchemaRecursive applies OpenAI normalization recursively
func normalizeSchemaRecursive(schema map[string]interface{}) map[string]interface{} {
	// Remove unsupported format values (OpenAI only supports a limited set)
	if format, ok := schema["format"].(string); ok {
		// OpenAI supported formats: date-time, date, time, email
		// Remove uri, uri-reference, hostname, ipv4, ipv6, uuid, etc.
		switch format {
		case "date-time", "date", "time", "email":
			// Keep these
		default:
			delete(schema, "format")
		}
	}

	// Handle properties
	if props, ok := schema["properties"].(map[string]interface{}); ok && len(props) > 0 {
		// Recursively normalize each property
		for key, val := range props {
			if propSchema, ok := val.(map[string]interface{}); ok {
				props[key] = normalizeSchemaRecursive(propSchema)
			}
		}

		// Build required array with all property keys
		required := make([]string, 0, len(props))
		for key := range props {
			required = append(required, key)
		}
		schema["required"] = required
	}

	// Handle array items
	if items, ok := schema["items"].(map[string]interface{}); ok {
		schema["items"] = normalizeSchemaRecursive(items)
	}

	// Handle anyOf, oneOf, allOf
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for i, item := range arr {
				if itemSchema, ok := item.(map[string]interface{}); ok {
					arr[i] = normalizeSchemaRecursive(itemSchema)
				}
			}
		}
	}

	// OpenAI requires additionalProperties to be false for objects.
	// Exception: if additionalProperties is already a schema map (e.g. {"type":"string"}),
	// preserve it — that's a valid free-form map type (like the env parameter).
	if schema["type"] == "object" || schema["properties"] != nil {
		if _, isSchemaMap := schema["additionalProperties"].(map[string]interface{}); !isSchemaMap {
			schema["additionalProperties"] = false
		}
	}

	return schema
}

func collectRoleText(messages []Message, role Role) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role != role {
			continue
		}
		if text := collectTextParts(msg.Parts); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}
