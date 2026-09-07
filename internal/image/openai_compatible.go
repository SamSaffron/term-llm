package image

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// OpenAICompatibleProvider generates images through a configured Images API.
// It deliberately does not assume support for OpenAI-specific options or editing.
type OpenAICompatibleProvider struct {
	name, endpoint, apiKey, model string
}

func newConfiguredImageProvider(cfg *config.Config, name, model string) (ImageProvider, error) {
	pc := cfg.GetProviderConfig(name)
	if pc == nil {
		return nil, fmt.Errorf("unknown image provider: %s (use a built-in or configure a provider with type: openai_compatible)", name)
	}
	if config.InferProviderType(name, pc.Type) != config.ProviderTypeOpenAICompat {
		return nil, fmt.Errorf("image provider %q must have type: openai_compatible", name)
	}
	pc, err := cfg.GetResolvedProviderConfig(name)
	if err != nil {
		return nil, fmt.Errorf("image provider %q: %w", name, err)
	}
	if err = pc.ResolveForInference(); err != nil {
		return nil, fmt.Errorf("image provider %q: %w", name, err)
	}
	if model == "" {
		model = pc.Model
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("image provider %q requires a model", name)
	}
	// A full chat URL is not an Images API base. Never silently send to it.
	if pc.URL != "" || pc.ResolvedURL != "" {
		return nil, fmt.Errorf("image provider %q requires base_url, not the chat-only url field", name)
	}
	base, err := url.Parse(pc.BaseURL)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, fmt.Errorf("image provider %q requires an HTTP(S) base_url without credentials, query or fragment", name)
	}
	return &OpenAICompatibleProvider{name: name, endpoint: strings.TrimRight(base.String(), "/") + "/images/generations", apiKey: pc.ResolvedAPIKey, model: model}, nil
}

func (p *OpenAICompatibleProvider) Name() string             { return p.name }
func (p *OpenAICompatibleProvider) SupportsEdit() bool       { return false }
func (p *OpenAICompatibleProvider) SupportsMultiImage() bool { return false }
func (p *OpenAICompatibleProvider) Edit(context.Context, EditRequest) (*ImageResult, error) {
	return nil, fmt.Errorf("image provider %s does not support editing", p.name)
}
func (p *OpenAICompatibleProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	// Size is optional: local models can retain their native resolution. No GPT
	// quality/output_format parameters are imposed on compatible implementations.
	payload := struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n"`
		ResponseFormat string `json:"response_format"`
		Size           string `json:"size,omitempty"`
	}{Model: p.model, Prompt: req.Prompt, N: 1, ResponseFormat: "b64_json"}
	if req.Size != "" || req.AspectRatio != "" {
		payload.Size = openaiSizeFromRequest("gpt-image-2", req.Size, req.AspectRatio)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	return doOpenAIImageRequest(openaiHTTPClient, request)
}
