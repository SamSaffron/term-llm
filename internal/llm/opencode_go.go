package llm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
)

// OpenCodeGoProvider routes OpenCode Go models to the wire protocol advertised
// by the live OpenCode model catalog.
type OpenCodeGoProvider struct {
	apiKey     string
	model      string
	baseURL    string
	catalogURL string
	httpClient *http.Client
	catalog    *opencodeGoModelCatalog
	chat       *OpenAICompatProvider
	messages   *AnthropicProvider
	responses  *ResponsesClient
}

var openCodeGoCatalogs = struct {
	sync.RWMutex
	byScope map[string]*opencodeGoModelCatalog
	latest  *opencodeGoModelCatalog
}{byScope: make(map[string]*opencodeGoModelCatalog)}

func openCodeGoCatalogScope(apiKey, baseURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(baseURL, "/") + "\x00" + strings.TrimSpace(apiKey)))
	return fmt.Sprintf("%s-%x", opencodeGoModelCacheKey, sum[:8])
}

func sharedOpenCodeGoCatalog(apiKey, baseURL string) *opencodeGoModelCatalog {
	scope := openCodeGoCatalogScope(apiKey, baseURL)
	openCodeGoCatalogs.Lock()
	defer openCodeGoCatalogs.Unlock()
	catalog := openCodeGoCatalogs.byScope[scope]
	if catalog == nil {
		catalog = &opencodeGoModelCatalog{cacheKey: scope}
		openCodeGoCatalogs.byScope[scope] = catalog
	}
	openCodeGoCatalogs.latest = catalog
	return catalog
}

func latestOpenCodeGoCatalog() *opencodeGoModelCatalog {
	openCodeGoCatalogs.RLock()
	defer openCodeGoCatalogs.RUnlock()
	return openCodeGoCatalogs.latest
}

func NewOpenCodeGoProvider(apiKey, model string) *OpenCodeGoProvider {
	return newOpenCodeGoProvider(apiKey, model, opencodeGoBaseURL, opencodeGoCatalogURL, defaultHTTPClient)
}

func newOpenCodeGoProvider(apiKey, model, baseURL, catalogURL string, client *http.Client) *OpenCodeGoProvider {
	apiKey = strings.TrimSpace(apiKey)
	baseURL = strings.TrimRight(baseURL, "/")
	if client == nil {
		client = defaultHTTPClient
	}
	p := &OpenCodeGoProvider{
		apiKey:     apiKey,
		model:      model,
		baseURL:    baseURL,
		catalogURL: catalogURL,
		httpClient: client,
		catalog:    sharedOpenCodeGoCatalog(apiKey, baseURL),
	}
	p.chat = NewOpenAICompatProvider(baseURL, apiKey, model, opencodeGoDisplayName)
	if model = strings.TrimSpace(model); model != "" {
		// Preserve the configured ID exactly when catalog metadata is unavailable.
		// Generic OpenAI-compatible parsing would otherwise turn a natural model
		// such as qwen3.8-max into qwen3.8 plus effort=max.
		p.chat.modelConfigs = []config.ProviderModelConfig{{ID: model}}
	}
	p.messages = newOpenCodeGoAnthropicProvider(apiKey, baseURL, model, client)
	p.responses = &ResponsesClient{
		BaseURL:            baseURL + "/responses",
		GetAuthHeader:      func() string { return "Bearer " + apiKey },
		HTTPClient:         client,
		DisableServerState: true,
	}
	return p
}

func (p *OpenCodeGoProvider) Name() string {
	if strings.TrimSpace(p.model) == "" {
		return opencodeGoDisplayName
	}
	return fmt.Sprintf("%s (%s)", opencodeGoDisplayName, p.model)
}

func (p *OpenCodeGoProvider) Credential() string { return "api_key" }

func (p *OpenCodeGoProvider) Capabilities() Capabilities {
	return Capabilities{ToolCalls: true, SupportsToolChoice: true}
}

func (p *OpenCodeGoProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, _, err := p.ListModelsWithFreshness(ctx)
	return models, err
}

func (p *OpenCodeGoProvider) ListModelsWithFreshness(ctx context.Context) ([]ModelInfo, bool, error) {
	loaded, err := p.catalog.loadWithFreshness(ctx, p.httpClient, p.apiKey, p.baseURL, p.catalogURL)
	if err != nil {
		return nil, false, err
	}
	out := make([]ModelInfo, 0, len(loaded.models))
	for _, model := range loaded.models {
		if model.Deprecated {
			continue
		}
		info := model.ModelInfo
		info.ReasoningEfforts = append([]string(nil), info.ReasoningEfforts...)
		out = append(out, info)
	}
	return out, loaded.fresh, nil
}

func (p *OpenCodeGoProvider) RefreshModelMetadata(ctx context.Context) error {
	_, err := p.catalog.load(ctx, p.httpClient, p.apiKey, p.baseURL, p.catalogURL)
	return err
}

func (p *OpenCodeGoProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	selected := chooseModel(req.Model, p.model)
	if strings.TrimSpace(selected) == "" {
		return nil, fmt.Errorf("OpenCode Go model is required")
	}
	metadata, err := p.catalog.model(ctx, p.httpClient, p.apiKey, p.baseURL, p.catalogURL, selected)
	if err != nil {
		slog.Debug("OpenCode Go catalog unavailable; defaulting to Chat Completions", "model", selected, "error", err)
		metadata = unknownOpenCodeGoModel(selected)
	}
	model, effort := splitOpenCodeGoModelEffort(selected, metadata)
	req.Model = model
	if strings.TrimSpace(req.ReasoningEffort) == "" {
		req.ReasoningEffort = effort
	}
	if metadata.OutputLimit > 0 && req.MaxOutputTokens > metadata.OutputLimit {
		req.MaxOutputTokens = metadata.OutputLimit
	}

	switch metadata.Protocol {
	case opencodeGoProtocolMessages:
		// OpenCode Go does not expose Anthropic's native server tools; search and
		// fetch continue to use term-llm's portable tool loop.
		req.Search = false
		reasoningEffort := strings.TrimSpace(req.ReasoningEffort)
		budget := metadata.ReasoningBudgets[strings.ToLower(reasoningEffort)]
		if budget > 0 {
			reasoningEffort = ""
		}
		// The Messages API requires max_tokens. Match OpenCode's 32K default cap
		// rather than sending either Anthropic's generic fallback or a potentially
		// enormous catalog output limit.
		if req.MaxOutputTokens == 0 {
			req.MaxOutputTokens = metadata.OutputLimit
			if req.MaxOutputTokens <= 0 || req.MaxOutputTokens > opencodeGoOutputTokenMax {
				req.MaxOutputTokens = opencodeGoOutputTokenMax
			}
		}
		if req.DebugRaw {
			DebugRawSection(true, "OpenCode Go Routed Request", fmt.Sprintf("protocol: messages\nmodel: %s\nreasoning_effort: %s\nthinking_budget: %d\nmax_output_tokens: %d", req.Model, req.ReasoningEffort, budget, req.MaxOutputTokens))
		}
		// Budget-based non-Anthropic models accept `thinking`, but reject Claude's
		// separate `output_config.effort` field.
		return p.messages.streamStandardForModel(ctx, req, req.Model, reasoningEffort, budget, false, false)
	case opencodeGoProtocolResponses:
		return p.streamResponses(ctx, req)
	default:
		return p.chat.Stream(ctx, req)
	}
}

func splitOpenCodeGoModelEffort(model string, metadata opencodeGoModel) (string, string) {
	model = strings.TrimSpace(model)
	baseModel := strings.TrimSpace(metadata.ID)
	if strings.EqualFold(model, baseModel) && metadata.Available {
		return model, ""
	}
	for _, effort := range metadata.ReasoningEfforts {
		effort = strings.TrimSpace(effort)
		if effort != "" && strings.EqualFold(model, baseModel+"-"+effort) {
			return baseModel, effort
		}
	}
	if !metadata.Available {
		lower := strings.ToLower(model)
		if base, hasSuffix := trimKnownEffortSuffix(lower); hasSuffix {
			return model[:len(base)], model[len(base)+1:]
		}
	}
	return model, ""
}

func (p *OpenCodeGoProvider) streamResponses(ctx context.Context, req Request) (Stream, error) {
	tools := BuildResponsesTools(req.Tools)
	request := ResponsesRequest{
		Model:                          req.Model,
		Messages:                       req.Messages,
		IncludeDeveloperInContinuation: req.IncludeDeveloperInContinuation,
		Tools:                          tools,
		Include:                        []string{"reasoning.encrypted_content"},
		Store:                          boolPtr(false),
		Stream:                         true,
		SessionID:                      req.SessionID,
		ForceHTTP:                      true,
	}
	if req.ToolChoice.Mode != "" {
		request.ToolChoice = BuildResponsesToolChoice(req.ToolChoice)
	}
	if len(tools) > 0 {
		request.ParallelToolCalls = boolPtr(req.ParallelToolCalls)
	}
	if req.MaxOutputTokens > 0 {
		request.MaxOutputTokens = req.MaxOutputTokens
	}
	if req.TemperatureSet || req.Temperature != 0 {
		value := float64(req.Temperature)
		request.Temperature = &value
	}
	if req.TopPSet || req.TopP != 0 {
		value := float64(req.TopP)
		request.TopP = &value
	}
	if effort := strings.TrimSpace(req.ReasoningEffort); effort != "" {
		request.Reasoning = &ResponsesReasoning{Effort: effort, Summary: "auto"}
	}
	return p.responses.Stream(ctx, request, req.DebugRaw)
}
