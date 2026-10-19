package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// This file is the cross-package surface used by the parent llm package for
// the Bedrock, OpenCode Go, context-window and model-registry integrations.

// ModelSpec is a decoded model plus the options selected by its suffixes.
type ModelSpec struct {
	Model          string
	Effort         string
	ThinkingBudget int64 // 0 = disabled
	Adaptive       bool
	Use1M          bool
}

// NewExtraHeaders validates configured extra headers (anthropic-beta values
// are kept as headers; the transport decides where they go).
func NewExtraHeaders(headers map[string]string) (http.Header, error) {
	return newAnthropicExtraHeaders(headers)
}

// NewTransportProvider builds a provider that sends every request through t
// (AWS Bedrock). resolveID maps a suffix-stripped model name to its wire ID.
func NewTransportProvider(model string, headers http.Header, t Transport, resolveID func(string) string) *AnthropicProvider {
	spec := parseAnthropicModelSpec(model, resolveID)
	client := &anthropicClient{headers: headers, httpClient: newAnthropicHTTPClient(), bedrock: t}
	return &AnthropicProvider{
		client:          client,
		model:           spec.model,
		thinkingBudget:  spec.thinkingBudget,
		useAdaptive:     spec.adaptive,
		use1m:           spec.use1m,
		reasoningEffort: spec.effort,
		credential:      "aws",
		resolveModelID:  resolveID,
	}
}

// NewMessagesProvider builds a provider for an Anthropic-compatible Messages
// gateway (OpenCode Go). A trailing /v1 on baseURL is dropped because the
// client appends v1/messages itself; an invalid URL is reported on use
// instead of falling back to another host.
func NewMessagesProvider(apiKey, baseURL, model string, client *http.Client, invalidURLLabel string) *AnthropicProvider {
	if client == nil {
		client = newAnthropicHTTPClient()
	} else {
		// Never follow redirects with X-Api-Key; copy so a shared client is
		// left untouched.
		copied := *client
		copied.CheckRedirect = refuseAnthropicRedirect
		client = &copied
	}
	ac := &anthropicClient{apiKey: apiKey, httpClient: client}
	parsed, err := parseAnthropicBaseURL(strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1"))
	if err != nil {
		ac.baseErr = fmt.Errorf("%s %q: %w", invalidURLLabel, baseURL, err)
	} else {
		ac.baseURL = parsed
	}
	return &AnthropicProvider{client: ac, model: model, credential: "api_key"}
}

// Details reports the decoded model and thinking options for display names.
func (p *AnthropicProvider) Details() ModelSpec {
	return ModelSpec{Model: p.model, Effort: p.reasoningEffort, ThinkingBudget: p.thinkingBudget, Adaptive: p.useAdaptive, Use1M: p.use1m}
}

// StreamWithSpec streams req using an explicit model spec. extraHeaders carry
// per-request gateway attribution.
func (p *AnthropicProvider) StreamWithSpec(ctx context.Context, req core.Request, spec ModelSpec, includeOutputEffort bool, extraHeaders http.Header) (core.Stream, error) {
	return p.streamStandardForModel(ctx, req, anthropicModelSpec{
		model: spec.Model, effort: spec.Effort, thinkingBudget: spec.ThinkingBudget, adaptive: spec.Adaptive, use1m: spec.Use1M,
	}, includeOutputEffort, extraHeaders)
}

// ModelAlias normalizes a model ID to its family alias.
func ModelAlias(model string) string { return anthropicModelAlias(model) }

// CachedModelInfoForRequest returns live-catalog info for model, retrying
// without an effort suffix.
func CachedModelInfoForRequest(model string) (core.ModelInfo, bool) {
	return anthropicCachedModelInfoForRequest(model)
}

// ReasoningEfforts returns the effort levels for a direct Anthropic model.
func ReasoningEfforts(model string) []string { return anthropicReasoningEfforts(model) }

// ClampOutputTokensForRequest caps requested output at the live catalog's
// limit for model, falling back to the curated limit.
func ClampOutputTokensForRequest(requested int, model string) int {
	return anthropicClampOutputTokens(requested, model)
}
