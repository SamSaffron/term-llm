package gemini

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// Options configures a Provider for gateway-fronted deployments.
type Options struct {
	// BaseURL is the ".../models" endpoint root. Empty selects the public API.
	BaseURL string
	// Client is the HTTP client. Nil selects a streaming-safe default.
	Client *http.Client
	// ExtraHeaders supplies per-request headers (gateway attribution/auth).
	ExtraHeaders func(ctx context.Context) map[string]string
	// AllowThinkingWithTools permits thinking config alongside function tools.
	AllowThinkingWithTools bool
	// ClampOutputTokens caps the requested output budget for a model. Nil
	// leaves the request unchanged; the llm root passes llm.ClampOutputTokens.
	ClampOutputTokens func(requested int, model string) int
}

// ThinkingLevel is a Gemini 3+ thinking level.
type ThinkingLevel = geminiThinkingLevel

// New creates a Gemini provider for the public Gemini API.
func New(apiKey, model string) *Provider {
	return NewWithOptions(apiKey, model, Options{})
}

// NewWithOptions creates a Gemini provider with gateway-specific options.
func NewWithOptions(apiKey, model string, opts Options) *Provider {
	if model == "" {
		model = config.DefaultProviderModel("gemini")
	}
	baseModel, thinkingCfg := parseGeminiModelThinking(model)
	p := &Provider{
		apiKey:                 apiKey,
		model:                  baseModel,
		thinkingLevel:          thinkingCfg.level,
		thinkingBudget:         thinkingCfg.budget,
		baseURL:                geminiAPIBaseURL,
		client:                 opts.Client,
		extraHeaders:           opts.ExtraHeaders,
		allowThinkingWithTools: opts.AllowThinkingWithTools,
		clampOutputTokens:      opts.ClampOutputTokens,
	}
	if opts.BaseURL != "" {
		p.baseURL = opts.BaseURL
	}
	if p.client == nil {
		p.client = defaultHTTPClient
	}
	return p
}

// WithRequestConfig returns a copy of p with the per-request model, thinking
// level, thinking budget, and header supplier replaced. The receiver is not
// modified, so a shared transport can be cloned per concurrent stream. An
// empty level and nil budget send no explicit thinking config.
func (p *Provider) WithRequestConfig(model string, level ThinkingLevel, budget *int32, headers func(context.Context) map[string]string) *Provider {
	clone := *p
	clone.model = model
	clone.thinkingLevel = level
	clone.thinkingBudget = budget
	clone.extraHeaders = headers
	return &clone
}

// Model returns the configured base model name.
func (p *Provider) Model() string { return p.model }

// defaultHTTPClient mirrors the llm root streaming client: transport-level
// timeouts only, so long-running streams are never killed.
var defaultHTTPClient = newStreamingHTTPClient()

func newStreamingHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 100
	return &http.Client{Transport: transport}
}

func setNonEmptyHeaders(header http.Header, values map[string]string) {
	for key, value := range values {
		if value != "" {
			header.Set(key, value)
		}
	}
}

// prependTextToParts prepends prefix to the first text part in parts, or
// inserts a new text part at the front if none exists.
func prependTextToParts(prefix string, parts []core.Part) []core.Part {
	for i, p := range parts {
		if p.Type == core.PartText {
			out := make([]core.Part, len(parts))
			copy(out, parts)
			out[i].Text = prefix + out[i].Text
			return out
		}
	}
	return append([]core.Part{{Type: core.PartText, Text: prefix}}, parts...)
}

func (p *Provider) clampTokens(requested int, model string) int {
	if p.clampOutputTokens == nil {
		return requested
	}
	return p.clampOutputTokens(requested, model)
}

// BaseURL returns the configured ".../models" endpoint root.
func (p *Provider) BaseURL() string { return p.baseURL }
