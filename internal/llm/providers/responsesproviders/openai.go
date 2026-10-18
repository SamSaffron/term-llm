package responsesproviders

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/openaiapi"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// OpenAIProvider implements Provider using the standard OpenAI API.
type OpenAIProvider struct {
	endpoint                  openaiapi.Endpoint // Resolved API root plus organization/project scoping
	apiKey                    string
	model                     string
	effort                    string            // reasoning effort: "low", "medium", "high", "xhigh", or ""
	useWebSocket              bool              // Responses-over-WebSocket transport
	serviceTier               string            // Optional Responses API service tier default
	fileUploadPolicy          *FileUploadPolicy // Provider-specific native file forwarding policy
	disableServerState        bool              // Send full history instead of previous_response_id
	disableEncryptedReasoning bool              // Omit the encrypted reasoning include for incompatible endpoints
	responsesOptions          ResponsesOptions
	responsesClient           *ResponsesClient // Shared client for Responses API with server state
	providerName              string           // Config provider key this instance was created for; "" means built-in parsing only
}

type OpenAIProviderOptions struct {
	// Endpoint overrides the API root, organization and project. Blank fields
	// fall back to OPENAI_BASE_URL, OPENAI_ORG_ID and OPENAI_PROJECT_ID.
	Endpoint                  openaiapi.Endpoint
	UseWebSocket              bool
	ServiceTier               string
	FileUploadPolicy          *FileUploadPolicy
	UsePreviousResponseID     *bool // nil preserves the default: server-state continuation
	RequestEncryptedReasoning *bool // nil preserves the default: request encrypted reasoning
	Responses                 ResponsesOptions
	// ProviderName is the config provider key this instance was created for
	// (e.g. "muse" for a custom-named type: openai provider). Effort suffixes
	// first resolve against that provider's config-declared reasoning_efforts;
	// blank skips the config lookup and uses only built-in OpenAI parsing.
	ProviderName string
}

// parseOpenAIModelEffort splits a model/effort suffix for an OpenAI Responses
// provider: efforts declared for the configured provider name (via
// models[].reasoning_efforts) win, then OpenAI's built-in effort tables apply
// so GPT model suffixes keep working under custom provider names.
func parseOpenAIModelEffort(providerName, model string) (string, string) {
	if base, effort, _ := configBaseModelAndEffortForProvider(providerName, model); base != "" {
		return base, effort
	}
	return parseModelEffortForProvider("openai", model)
}

// ParseModelEffort extracts effort suffix from model name.
// "gpt-5.2-high" -> ("gpt-5.2", "high")
// "gpt-5.2-xhigh" -> ("gpt-5.2", "xhigh")
// "gpt-5.2" -> ("gpt-5.2", "")
func ParseModelEffort(model string) (string, string) {
	// Check suffixes in order from longest to shortest to avoid "-high" matching "-xhigh"
	suffixes := []string{"minimal", "medium", "xhigh", "high", "low", "max"}
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
	actualModel, effort := parseOpenAIModelEffort(opts.ProviderName, model)
	return &OpenAIProvider{
		providerName:              opts.ProviderName,
		endpoint:                  openaiapi.Resolve(opts.Endpoint),
		apiKey:                    apiKey,
		model:                     actualModel,
		effort:                    effort,
		useWebSocket:              opts.UseWebSocket,
		serviceTier:               NormalizeServiceTier(opts.ServiceTier),
		fileUploadPolicy:          cloneFileUploadPolicy(opts.FileUploadPolicy),
		disableServerState:        opts.UsePreviousResponseID != nil && !*opts.UsePreviousResponseID,
		disableEncryptedReasoning: opts.RequestEncryptedReasoning != nil && !*opts.RequestEncryptedReasoning,
		responsesOptions:          opts.Responses,
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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint.URL("models"), nil)
	if err != nil {
		return nil, fmt.Errorf("create OpenAI models request: %w", err)
	}
	p.endpoint.Apply(httpReq.Header)
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := defaultHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read OpenAI models response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list models: %w", newHTTPStatusError("OpenAI", resp, body))
	}

	var modelsResp oaiModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return nil, fmt.Errorf("parse OpenAI models response: %w", err)
	}

	models := make([]ModelInfo, 0, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
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
			BaseURL:            p.endpoint.URL("responses"),
			GetAuthHeader:      func() string { return "Bearer " + p.apiKey },
			ExtraHeaders:       p.endpoint.HeaderMap(),
			HTTPClient:         defaultHTTPClient,
			UseWebSocket:       p.useWebSocket,
			DisableServerState: p.disableServerState,
		}
	}

	// Effort precedence: req.ReasoningEffort wins over model suffix, which wins over provider-level effort.
	reqModel, reqEffort := parseOpenAIModelEffort(p.providerName, req.Model)
	model := chooseModel(reqModel, p.model)
	effort := p.effort
	if reqEffort != "" {
		effort = reqEffort
	}
	if v := strings.TrimSpace(req.ReasoningEffort); v != "" {
		effort = v
	}

	responsesOptions := mergeResponsesOptions(p.responsesOptions, req.Responses, req.Ephemeral)
	responsesOptions, err := validateResponsesOptions("openai", model, &responsesOptions, req.Tools)
	if err != nil {
		return nil, err
	}

	// Build tools - add web search tool first if requested.
	tools := BuildResponsesToolsWithOptions(req.Tools, responsesOptions.ProgrammaticToolCalling)
	if req.Search {
		webSearchTool := ResponsesWebSearchTool{Type: "web_search_preview"}
		tools = append([]any{webSearchTool}, tools...)
	}

	responsesReq := ResponsesRequest{
		Model:                          model,
		Messages:                       req.Messages,
		IncludeDeveloperInContinuation: req.IncludeDeveloperInContinuation,
		FileUploadPolicy:               p.effectiveFileUploadPolicy(),
		Tools:                          tools,
		Stream:                         true,
		SessionID:                      req.SessionID,
	}

	if !p.disableEncryptedReasoning {
		responsesReq.Include = []string{"reasoning.encrypted_content"}
	}

	if serviceTier := p.serviceTier; req.ServiceTierSet || strings.TrimSpace(req.ServiceTier) != "" {
		serviceTier = NormalizeServiceTier(req.ServiceTier)
		if serviceTier != "" {
			responsesReq.ServiceTier = serviceTier
		}
	} else if serviceTier != "" {
		responsesReq.ServiceTier = serviceTier
	}

	if req.ToolChoice.Mode != "" {
		responsesReq.ToolChoice = BuildResponsesToolChoice(req.ToolChoice)
	}
	guardReplayedWebSearch(&responsesReq, scanResponsesHistory(req.Messages))
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
	responsesReq.Reasoning = &ResponsesReasoning{}
	if !responsesOptions.MultiAgent.Enabled {
		responsesReq.Reasoning.Summary = "auto"
	}
	if effort != "" {
		responsesReq.Reasoning.Effort = effort
	}
	responsesReq.Reasoning.Mode = responsesOptions.ReasoningMode
	responsesReq.Reasoning.Context = responsesOptions.ReasoningContext
	if responsesOptions.MultiAgent.Enabled {
		responsesReq.MultiAgent = &ResponsesMultiAgent{
			Enabled:                true,
			MaxConcurrentSubagents: responsesOptions.MultiAgent.MaxConcurrentSubagents,
		}
		// Multi-agent WebSocket streams require response.inject support. Until
		// that bidirectional protocol is implemented, use the fully supported
		// HTTP/SSE transport for these requests even when WebSocket is configured.
		responsesReq.ForceHTTP = true
		responsesReq.ExtraHeaders = map[string]string{"OpenAI-Beta": "responses_multi_agent=v1"}
	}
	if responsesOptions.PromptCache.Mode != "" || responsesOptions.PromptCache.TTL != "" {
		responsesReq.PromptCacheOptions = &ResponsesPromptCacheOptions{Mode: responsesOptions.PromptCache.Mode, TTL: responsesOptions.PromptCache.TTL}
	}
	if responsesReq.Reasoning.Effort == "" && responsesReq.Reasoning.Summary == "" && responsesReq.Reasoning.Mode == "" && responsesReq.Reasoning.Context == "" {
		responsesReq.Reasoning = nil
	}

	if req.Debug {
		responsesReq.Input = BuildResponsesInputWithFilePolicy(req.Messages, responsesReq.FileUploadPolicy)
		systemPreview := collectRoleText(req.Messages, RoleSystem)
		userPreview := collectRoleText(req.Messages, RoleUser)
		runtimeoutput.Printf("%s\n", "=== DEBUG: OpenAI Stream Request ===")
		runtimeoutput.Printf("Provider: %s\n", p.Name())
		runtimeoutput.Printf("Developer: %s\n", truncate(systemPreview, 200))
		runtimeoutput.Printf("User: %s\n", truncate(userPreview, 200))
		runtimeoutput.Printf("Input Items: %d\n", len(responsesReq.Input))
		runtimeoutput.Printf("Tools: %d\n", len(tools))
		runtimeoutput.Printf("%s\n", "===================================")
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

func (p *OpenAIProvider) isolateHelperConversation() Provider {
	clone := *p
	clone.responsesClient = cloneResponsesClientFreshConversation(p.responsesClient)
	return &clone
}

func (p *OpenAIProvider) forkHelperConversation() (Provider, bool) {
	client, ok := cloneResponsesClientForkConversation(p.responsesClient)
	if !ok {
		return nil, false
	}
	clone := *p
	clone.responsesClient = client
	// A handover fork reuses only the server-side conversation boundary. It must
	// not inherit live execution features such as multi-agent or programmatic
	// tool calling, which can make a one-shot briefing invoke unrelated tools.
	clone.responsesOptions = ResponsesOptions{}
	return &clone, true
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
