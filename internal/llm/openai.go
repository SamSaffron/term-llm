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
	useWebSocket    bool             // Responses-over-WebSocket transport (default for built-in OpenAI configs)
	responsesClient *ResponsesClient // Shared client for Responses API with server state
}

type OpenAIProviderOptions struct {
	UseWebSocket bool
}

// ParseModelEffort extracts effort suffix from model name.
// "gpt-5.2-high" -> ("gpt-5.2", "high")
// "gpt-5.2-xhigh" -> ("gpt-5.2", "xhigh")
// "gpt-5.2" -> ("gpt-5.2", "")
func ParseModelEffort(model string) (string, string) {
	// Check suffixes in order from longest to shortest to avoid "-high" matching "-xhigh"
	suffixes := []string{"xhigh", "minimal", "medium", "high", "low", "max"}
	for _, effort := range suffixes {
		suffix := "-" + effort
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), effort
		}
	}
	return model, ""
}

func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	return NewOpenAIProviderWithOptions(apiKey, model, OpenAIProviderOptions{})
}

func NewOpenAIProviderWithOptions(apiKey, model string, opts OpenAIProviderOptions) *OpenAIProvider {
	actualModel, effort := ParseModelEffort(model)
	client := openai.NewClient(option.WithAPIKey(apiKey))
	return &OpenAIProvider{
		client:       &client,
		apiKey:       apiKey,
		model:        actualModel,
		effort:       effort,
		useWebSocket: opts.UseWebSocket,
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
			UseWebSocket:  p.useWebSocket,
		}
	}

	// Effort precedence: req.ReasoningEffort wins over model suffix, which wins over provider-level effort.
	reqModel, reqEffort := ParseModelEffort(req.Model)
	model := chooseModel(reqModel, p.model)
	effort := p.effort
	if reqEffort != "" {
		effort = reqEffort
	}
	if v := strings.TrimSpace(req.ReasoningEffort); v != "" {
		effort = v
	}

	// Build tools - add web search tool first if requested
	tools := BuildResponsesTools(req.Tools)
	if req.Search {
		webSearchTool := ResponsesWebSearchTool{Type: "web_search_preview"}
		tools = append([]any{webSearchTool}, tools...)
	}

	responsesReq := ResponsesRequest{
		Model:          model,
		Messages:       req.Messages,
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
	if req.TemperatureSet || req.Temperature != 0 {
		v := float64(req.Temperature)
		responsesReq.Temperature = &v
	}
	if req.TopPSet || req.TopP != 0 {
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
		responsesReq.Input = BuildResponsesInput(req.Messages)
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

// normalizeSchemaForOpenAIStrict applies normalizeSchemaForOpenAI and additionally
// converts free-form map properties (additionalProperties: schema) into arrays of
// key/value objects, which is required for strict mode where additionalProperties
// must be false on every object.
func normalizeSchemaForOpenAIStrict(schema map[string]interface{}) map[string]interface{} {
	return normalizeFreeFormMapProperties(normalizeSchemaForOpenAI(schema))
}

func defaultResponsesParametersSchema(schema map[string]interface{}) map[string]interface{} {
	if len(schema) > 0 {
		return schema
	}
	return map[string]interface{}{
		"type":                 "object",
		"properties":           map[string]interface{}{},
		"required":             []string{},
		"additionalProperties": false,
	}
}

// normalizeFreeFormMapProperties converts any free-form map schema (one whose
// additionalProperties is a schema object, not a bool) into an array of
// {key, value} pair objects. OpenAI strict mode requires additionalProperties:
// false on every object, so this is the closest strict-compatible equivalent.
// The function handles both the case where the current schema is itself a
// free-form map and the case where one is nested inside properties, items,
// anyOf, oneOf, or allOf.
func normalizeFreeFormMapProperties(schema map[string]interface{}) map[string]interface{} {
	// If this schema is itself a free-form map, convert it and return early.
	if valueSchema, isSchemaMap := schema["additionalProperties"].(map[string]interface{}); isSchemaMap {
		return convertFreeFormMapToArray(schema, valueSchema)
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
		"type": true, "properties": true, "required": true, "additionalProperties": true, "propertyNames": true,
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

func normalizedJSONSchemaTypeNames(value interface{}) ([]string, bool) {
	if value == nil {
		return nil, false
	}
	seen := map[string]bool{}
	var names []string
	add := func(s string) {
		if !isSupportedJSONSchemaType(s) || seen[s] {
			return
		}
		seen[s] = true
		names = append(names, s)
	}
	switch v := value.(type) {
	case string:
		add(v)
	case []string:
		for _, item := range v {
			add(item)
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	default:
		return nil, true
	}
	return names, true
}

func isSupportedJSONSchemaType(s string) bool {
	switch s {
	case "string", "number", "boolean", "integer", "object", "array", "null":
		return true
	default:
		return false
	}
}

func inferJSONSchemaType(schema map[string]interface{}) string {
	if _, ok := schema["anyOf"]; ok {
		return ""
	}
	if _, ok := schema["oneOf"]; ok {
		return ""
	}
	if _, ok := schema["allOf"]; ok {
		return ""
	}
	if schema["properties"] != nil || schema["required"] != nil || schema["additionalProperties"] != nil {
		return "object"
	}
	if schema["items"] != nil || schema["prefixItems"] != nil {
		return "array"
	}
	if schema["enum"] != nil || schema["format"] != nil {
		return "string"
	}
	for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
		if schema[key] != nil {
			return "number"
		}
	}
	return "string"
}

func normalizeSchemaTypeUnion(schema map[string]interface{}, typeNames []string) map[string]interface{} {
	anyOf := make([]interface{}, 0, len(typeNames))
	enumValue, hasEnum := schema["enum"]
	for _, typeName := range typeNames {
		branch := map[string]interface{}{"type": typeName}
		copyTypeSpecificSchemaKeywords(branch, schema, typeName)
		if hasEnum && typeName != "null" {
			branch["enum"] = enumValue
		}
		anyOf = append(anyOf, normalizeSchemaRecursive(branch))
	}

	for _, key := range []string{
		"type", "enum", "items", "prefixItems", "properties", "required", "additionalProperties",
		"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	} {
		delete(schema, key)
	}
	schema["anyOf"] = anyOf
	return schema
}

func copyTypeSpecificSchemaKeywords(dst, src map[string]interface{}, typeName string) {
	switch typeName {
	case "object":
		for _, key := range []string{"properties", "required", "additionalProperties"} {
			if val, ok := src[key]; ok {
				dst[key] = deepCopySchemaValue(val)
			}
		}
	case "array":
		for _, key := range []string{"items", "prefixItems"} {
			if val, ok := src[key]; ok {
				dst[key] = deepCopySchemaValue(val)
			}
		}
	case "number", "integer":
		for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
			if val, ok := src[key]; ok {
				dst[key] = val
			}
		}
	}
}

func deepCopySchemaValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		return deepCopyMap(val)
	case []interface{}:
		return deepCopySlice(val)
	default:
		return val
	}
}

// normalizeSchemaRecursive applies OpenAI normalization recursively
func normalizeSchemaRecursive(schema map[string]interface{}) map[string]interface{} {
	// MCP servers often provide broad JSON Schema. OpenAI's strict tool schema
	// subset is narrower: `type` must be a single primitive string, and nullable
	// or multi-type schemas must be represented with anyOf. JSON-decoded MCP
	// schemas also arrive as []interface{}, not []string, so normalize both.
	if typeNames, hadType := normalizedJSONSchemaTypeNames(schema["type"]); hadType {
		if len(typeNames) == 0 {
			delete(schema, "type")
		} else if len(typeNames) == 1 {
			schema["type"] = typeNames[0]
		} else {
			return normalizeSchemaTypeUnion(schema, typeNames)
		}
	}

	// Infer a missing or unusable type from common schema keywords. This mirrors
	// Codex's behavior for real-world MCP schemas such as Playwright, where some
	// properties can have an invalid/empty `type` but still include `items`,
	// `properties`, `additionalProperties`, `enum`, or numeric bounds.
	if _, hasType := schema["type"]; !hasType {
		if inferred := inferJSONSchemaType(schema); inferred != "" {
			schema["type"] = inferred
		}
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

	// Handle properties
	if props, ok := schema["properties"].(map[string]interface{}); ok && len(props) > 0 {
		// Recursively normalize each property. Boolean schemas are valid JSON Schema
		// but not valid OpenAI tool parameter schemas, so coerce them to a broad
		// string schema like Codex does for accept-all boolean schema forms.
		for key, val := range props {
			switch propSchema := val.(type) {
			case map[string]interface{}:
				props[key] = normalizeSchemaRecursive(propSchema)
			case bool:
				props[key] = map[string]interface{}{"type": "string"}
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
	} else if _, ok := schema["items"].(bool); ok {
		schema["items"] = map[string]interface{}{"type": "string"}
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

	// Handle additionalProperties schema form (free-form maps).
	if additionalProperties, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		schema["additionalProperties"] = normalizeSchemaRecursive(additionalProperties)
	}

	// Drop unsupported object-shape keywords that commonly appear in MCP schemas
	// but are rejected by OpenAI's tool-parameter schema subset.
	delete(schema, "propertyNames")

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
