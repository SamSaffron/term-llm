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
	req.MaxOutputTokens = ClampOutputTokens(req.MaxOutputTokens, chooseModel(req.Model, p.model))
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
	if len(tools) > 0 {
		responsesReq.ParallelToolCalls = boolPtr(req.ParallelToolCalls)
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

// normalizeSchemaForOpenAI ensures schema meets OpenAI's requirements:
// - 'required' must include every key in properties
// - 'additionalProperties' must be false (free-form maps are preserved as-is)
// - unsupported 'format' values must be removed
func normalizeSchemaForOpenAI(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return schema
	}
	return normalizeSchemaRecursive(deepCopyMap(schema))
}

// normalizeSchemaForOpenAIStrict applies normalizeSchemaForOpenAI and additionally:
//  1. Makes originally-optional properties nullable (anyOf with null) so the LLM
//     knows it can pass null — strict mode requires all properties in required.
//  2. Converts free-form map properties (additionalProperties: schema) into arrays
//     of key/value objects, since strict mode requires additionalProperties: false.
func normalizeSchemaForOpenAIStrict(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return schema
	}
	return normalizeFreeFormMapProperties(normalizeSchemaForOpenAI(schema))
}

// normalizeFreeFormMapProperties converts any free-form map schema (one whose
// additionalProperties is a schema object, not a bool) into an array of
// {key, value} pair objects. OpenAI strict mode requires additionalProperties:
// false on every object, so this is the closest strict-compatible equivalent.
// The function handles both the case where the current schema is itself a
// free-form map and the case where one is nested inside properties, items,
// anyOf, oneOf, or allOf.
func normalizeFreeFormMapProperties(schema map[string]interface{}) map[string]interface{} {
	// If this schema is itself a free-form map with a typed value schema,
	// convert it to an array of key/value objects for strict mode.
	// An empty schema map ({}) just means "accept any" — treat it as
	// additionalProperties: false rather than converting to array.
	if valueSchema, isSchemaMap := schema["additionalProperties"].(map[string]interface{}); isSchemaMap {
		if len(valueSchema) > 0 {
			return convertFreeFormMapToArray(schema, valueSchema)
		}
		schema["additionalProperties"] = false
	}

	// Recurse into properties.
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for key, val := range props {
			if propSchema, ok := val.(map[string]interface{}); ok {
				props[key] = normalizeFreeFormMapProperties(propSchema)
			}
		}
	}

	// Recurse into array items.
	if items, ok := schema["items"].(map[string]interface{}); ok {
		schema["items"] = normalizeFreeFormMapProperties(items)
	}

	// Recurse into anyOf, oneOf, allOf.
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for i, item := range arr {
				if itemSchema, ok := item.(map[string]interface{}); ok {
					arr[i] = normalizeFreeFormMapProperties(itemSchema)
				}
			}
		}
	}

	return schema
}

// convertFreeFormMapToArray transforms a free-form map schema (type:object with
// additionalProperties: schema) into a strict-compatible array of {key, value}
// objects. The original additionalProperties schema is preserved as the value
// type. All non-conflicting metadata fields (title, default, examples, etc.)
// from the original schema are copied to the result.
func convertFreeFormMapToArray(orig map[string]interface{}, valueSchema map[string]interface{}) map[string]interface{} {
	normalizedValue := normalizeFreeFormMapProperties(valueSchema)
	result := map[string]interface{}{
		"type": "array",
		"items": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"key":   map[string]interface{}{"type": "string"},
				"value": normalizedValue,
			},
			"required":             []string{"key", "value"},
			"additionalProperties": false,
		},
	}
	// Copy metadata not rewritten by the conversion (e.g. title, default, examples).
	skip := map[string]bool{
		"type": true, "properties": true, "required": true, "additionalProperties": true,
	}
	for k, v := range orig {
		if !skip[k] {
			result[k] = v
		}
	}
	return result
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
	// Remove JSON Schema keywords not supported by OpenAI function calling.
	for _, kw := range []string{
		"propertyNames", "dependentRequired", "dependentSchemas",
		"patternProperties", "unevaluatedProperties", "unevaluatedItems",
		"if", "then", "else",
		"$id", "$ref", "$schema", "$defs", "definitions", "$anchor",
		"$dynamicRef", "$dynamicAnchor",
		"contentMediaType", "contentEncoding",
		"prefixItems", "contains", "minContains", "maxContains",
		"not",
	} {
		delete(schema, kw)
	}

	// Normalize []interface{} type arrays (from JSON unmarshal) to []string
	// so the union type handler below can process them.
	if typeArr, ok := schema["type"].([]interface{}); ok {
		typeSlice := make([]string, 0, len(typeArr))
		for _, t := range typeArr {
			if s, ok := t.(string); ok {
				typeSlice = append(typeSlice, s)
			}
		}
		schema["type"] = typeSlice
	}

	// Handle union types expressed as []string (e.g. []string{"string", "null"}).
	// OpenAI strict mode requires anyOf instead of array type values.
	// Note: do NOT return early — the schema may have other keywords
	// (properties, additionalProperties, items) that still need processing.
	if typeSlice, ok := schema["type"].([]string); ok {
		anyOf := make([]interface{}, 0, len(typeSlice))
		enum, hasEnum := schema["enum"]
		delete(schema, "enum")
		delete(schema, "type")
		for _, t := range typeSlice {
			branch := map[string]interface{}{"type": t}
			if hasEnum && t != "null" {
				branch["enum"] = enum
			}
			anyOf = append(anyOf, branch)
		}
		schema["anyOf"] = anyOf
	}

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

	// Ensure objects have a properties key (required for OpenAI strict mode).
	// Without it, the property is treated as invalid and stripped.
	if schema["type"] == "object" && schema["properties"] == nil {
		schema["properties"] = map[string]interface{}{}
	}

	// Handle properties: recurse into each and rebuild required from actual keys.
	// Always sync required with properties to remove stale entries.
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for key, val := range props {
			if propSchema, ok := val.(map[string]interface{}); ok {
				props[key] = normalizeSchemaRecursive(propSchema)
			}
		}
		if len(props) > 0 {
			required := make([]string, 0, len(props))
			for key := range props {
				required = append(required, key)
			}
			schema["required"] = required
		} else {
			delete(schema, "required")
		}
	} else {
		// No properties — any leftover required is invalid
		delete(schema, "required")
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

	// Always recurse into additionalProperties when it's a schema map, so that
	// nested value schemas get required arrays, additionalProperties: false, etc.
	// For object schemas without a schema-valued additionalProperties, set it to false
	// as required by OpenAI.
	if addProps, isSchemaMap := schema["additionalProperties"].(map[string]interface{}); isSchemaMap {
		schema["additionalProperties"] = normalizeSchemaRecursive(addProps)
	} else if schema["type"] == "object" || schema["properties"] != nil {
		schema["additionalProperties"] = false
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
