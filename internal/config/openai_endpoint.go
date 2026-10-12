package config

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/openaiapi"
)

// OpenAIEndpoint returns this provider entry's OpenAI Platform endpoint:
// base_url (falling back to OPENAI_BASE_URL, then the public API root) and
// headers (with OpenAI-Organization/OpenAI-Project falling back to
// OPENAI_ORG_ID/OPENAI_PROJECT_ID). Lazy values must already be resolved with
// ResolveForInference.
func (cfg *ProviderConfig) OpenAIEndpoint() openaiapi.Endpoint {
	if cfg == nil {
		return openaiapi.Resolve(openaiapi.Endpoint{})
	}
	return openaiapi.Resolve(openaiapi.Endpoint{BaseURL: cfg.BaseURL, Headers: cfg.Headers})
}

// OpenAIEndpoint returns the OpenAI Platform endpoint for API-key features
// without their own provider entry (embeddings, images, transcription). It
// reuses providers.openai base_url and headers when that entry is the native
// OpenAI provider, so one setting scopes every OpenAI API-key request. Only
// those fields are resolved; the entry's api_key is left untouched.
func (c *Config) OpenAIEndpoint() (openaiapi.Endpoint, error) {
	var configured openaiapi.Endpoint
	name := string(ProviderTypeOpenAI)
	if pc := c.GetProviderConfig(name); pc != nil && InferProviderType(name, pc.Type) == ProviderTypeOpenAI {
		baseURL, err := Cred(pc.BaseURL).Resolve()
		if err != nil {
			return openaiapi.Endpoint{}, fmt.Errorf("providers.openai.base_url: %w", err)
		}
		headers, err := ResolveHeaderValues(pc.Headers)
		if err != nil {
			return openaiapi.Endpoint{}, fmt.Errorf("providers.openai.%w", err)
		}
		configured = openaiapi.Endpoint{BaseURL: baseURL, Headers: headers}
	}
	return openaiapi.Resolve(configured), nil
}

// ResolveHeaderValues resolves each configured header value (${ENV}, $(),
// op://, file://, srv://). Headers whose value resolves to blank are dropped.
// Errors name the failing header as "headers.<name>: ..." without its value.
func ResolveHeaderValues(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(headers))
	for key, raw := range headers {
		value, err := Cred(raw).Resolve()
		if err != nil {
			return nil, fmt.Errorf("headers.%s: %w", key, err)
		}
		if value != "" {
			resolved[key] = value
		}
	}
	return resolved, nil
}

// ResolveOpenAIHeaders resolves configured header values and adds
// OpenAI-Organization/OpenAI-Project from OPENAI_ORG_ID/OPENAI_PROJECT_ID when
// they are not configured. It is for OpenAI API-key clients that keep their
// own base URL setting (live voice, classify).
func ResolveOpenAIHeaders(headers map[string]string) (map[string]string, error) {
	resolved, err := ResolveHeaderValues(headers)
	if err != nil {
		return nil, err
	}
	return openaiapi.Resolve(openaiapi.Endpoint{Headers: resolved}).Headers, nil
}
