package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/samsaffron/term-llm/internal/config"
)

// OpenCode expects every request to carry a client identifier plus a stable
// per-conversation session ID so it can route and optimize traffic; requests
// missing the session header are rejected.
const (
	openCodeGoClientHeader  = "x-opencode-client"
	openCodeGoSessionHeader = "x-opencode-session"
	openCodeGoClientID      = "term-llm"
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

var (
	openCodeGoFallbackSessionOnce sync.Once
	openCodeGoFallbackSession     string

	openCodeGoCatalogs = struct {
		sync.RWMutex
		byScope map[string]*opencodeGoModelCatalog
		latest  *opencodeGoModelCatalog
	}{byScope: make(map[string]*opencodeGoModelCatalog)}
)

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
	return NewOpenCodeGoProviderWithBaseURL(apiKey, model, "")
}

// NewOpenCodeGoProviderWithBaseURL creates an OpenCode Go provider using an
// optional compatible proxy base URL.
func NewOpenCodeGoProviderWithBaseURL(apiKey, model, baseURL string) *OpenCodeGoProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = opencodeGoBaseURL
	}
	return newOpenCodeGoProvider(apiKey, model, baseURL, opencodeGoCatalogURL, defaultHTTPClient)
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
	// Chat Completions headers are built per request so each call carries the
	// session ID resolved by Stream.
	p.chat.requestHeaders = func(ctx context.Context) map[string]string {
		return p.requestHeaders(openCodeGoSessionFromContext(ctx))
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

// opencodeGoSessionKey carries the resolved OpenCode session ID down to the
// Chat Completions transport, whose headers are applied per request. It is
// deliberately separate from the engine's session context value so a generated
// fallback ID never masquerades as a real session elsewhere.
type opencodeGoSessionKey struct{}

func withOpenCodeGoSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, opencodeGoSessionKey{}, sessionID)
}

func openCodeGoSessionFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(opencodeGoSessionKey{}).(string); ok {
		return id
	}
	return ""
}

// sessionID returns the session ID to advertise for a request, falling back to
// the process-wide ID when the caller supplied none or supplied one that cannot
// be sent as an HTTP header value.
func (p *OpenCodeGoProvider) sessionID(requested string) string {
	if validOpenCodeGoSessionID(requested) {
		return requested
	}
	return openCodeGoFallbackSessionID()
}

// requestHeaders returns OpenCode's client and session attribution headers.
func (p *OpenCodeGoProvider) requestHeaders(sessionID string) map[string]string {
	return openCodeGoAttributionHeaders(p.sessionID(sessionID))
}

// openCodeGoFallbackSessionID is a stable ID for OpenCode API calls that are
// not tied to a conversation: title generation, catalog fetches, and usage.
func openCodeGoFallbackSessionID() string {
	openCodeGoFallbackSessionOnce.Do(func() {
		openCodeGoFallbackSession = newOpenCodeGoSessionID()
	})
	return openCodeGoFallbackSession
}

func openCodeGoAttributionHeaders(sessionID string) map[string]string {
	if !validOpenCodeGoSessionID(sessionID) {
		sessionID = openCodeGoFallbackSessionID()
	}
	return map[string]string{
		openCodeGoClientHeader:  openCodeGoClientID,
		openCodeGoSessionHeader: sessionID,
	}
}

func applyOpenCodeGoAttributionHeaders(header http.Header, sessionID string) {
	setNonEmptyHeaders(header, openCodeGoAttributionHeaders(sessionID))
}

// validOpenCodeGoSessionID reports whether an ID is safe to send verbatim as a
// header value: non-empty, bounded, and free of control or non-ASCII bytes.
func validOpenCodeGoSessionID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func newOpenCodeGoSessionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s-%d", openCodeGoClientID, time.Now().UnixNano())
	}
	return openCodeGoClientID + "-" + hex.EncodeToString(raw[:])
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
	sessionID := p.sessionID(req.SessionID)
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
		return p.messages.streamStandardForModel(ctx, req, req.Model, reasoningEffort, budget, false, false,
			option.WithHeader(openCodeGoClientHeader, openCodeGoClientID),
			option.WithHeader(openCodeGoSessionHeader, sessionID))
	case opencodeGoProtocolResponses:
		return p.streamResponses(ctx, req, sessionID)
	default:
		return p.chat.Stream(withOpenCodeGoSession(ctx, sessionID), req)
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

func (p *OpenCodeGoProvider) streamResponses(ctx context.Context, req Request, sessionID string) (Stream, error) {
	tools := BuildResponsesTools(req.Tools)
	request := ResponsesRequest{
		ExtraHeaders:                   openCodeGoAttributionHeaders(sessionID),
		Model:                          req.Model,
		Messages:                       req.Messages,
		IncludeDeveloperInContinuation: req.IncludeDeveloperInContinuation,
		Tools:                          tools,
		Include:                        []string{"reasoning.encrypted_content"},
		Store:                          boolPtr(false),
		Stream:                         true,
		SessionID:                      sessionID,
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
