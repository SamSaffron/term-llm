// Package openaiapi resolves the public OpenAI API endpoint settings shared by
// every API-key OpenAI client (Responses, model listing, embeddings, images and
// transcription).
//
// The API root resolves from explicit configuration, then OPENAI_BASE_URL,
// then the public default. Request headers come from configuration; the
// OpenAI-Organization and OpenAI-Project headers fall back to OPENAI_ORG_ID and
// OPENAI_PROJECT_ID when configuration does not set them.
package openaiapi

import (
	"net/http"
	"os"
	"strings"
)

const (
	// DefaultBaseURL is the public OpenAI API root.
	DefaultBaseURL = "https://api.openai.com/v1"

	EnvBaseURL      = "OPENAI_BASE_URL"
	EnvOrganization = "OPENAI_ORG_ID"
	EnvProject      = "OPENAI_PROJECT_ID"

	HeaderOrganization = "OpenAI-Organization"
	HeaderProject      = "OpenAI-Project"
)

// Endpoint is an OpenAI API root plus extra request headers, such as the
// organization and project that scope API-key requests.
type Endpoint struct {
	BaseURL string
	Headers map[string]string
}

// Resolve defaults BaseURL from OPENAI_BASE_URL, then the public API root, and
// adds OpenAI-Organization/OpenAI-Project from OPENAI_ORG_ID/OPENAI_PROJECT_ID
// unless the configured headers already set them (case-insensitively). Blank
// configured header values and protocol-managed headers (Authorization,
// Content-Type, Accept) are dropped, so configuration can never replace the API
// key or a generated multipart boundary. The input map is not modified.
func Resolve(configured Endpoint) Endpoint {
	resolved := Endpoint{
		BaseURL: strings.TrimRight(firstNonEmpty(configured.BaseURL, os.Getenv(EnvBaseURL), DefaultBaseURL), "/"),
	}
	headers := make(map[string]string, len(configured.Headers)+2)
	present := make(map[string]bool, len(configured.Headers))
	for key, value := range configured.Headers {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if isReservedHeader(key) {
			continue
		}
		if key != "" && value != "" {
			headers[key] = value
			present[strings.ToLower(key)] = true
		}
	}
	for _, fallback := range []struct{ header, env string }{
		{HeaderOrganization, EnvOrganization},
		{HeaderProject, EnvProject},
	} {
		if present[strings.ToLower(fallback.header)] {
			continue
		}
		if value := strings.TrimSpace(os.Getenv(fallback.env)); value != "" {
			headers[fallback.header] = value
		}
	}
	if len(headers) > 0 {
		resolved.Headers = headers
	}
	return resolved
}

// Public returns the public API root without extra headers and without
// consulting the environment. It is for credentials, such as ChatGPT OAuth
// tokens, that are not OpenAI Platform API keys.
func Public() Endpoint {
	return Endpoint{BaseURL: DefaultBaseURL}
}

// URL joins path onto the endpoint's API root.
func (e Endpoint) URL(path string) string {
	base := strings.TrimRight(e.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

// HeaderMap returns a copy of the extra request headers, or nil if there are
// none.
func (e Endpoint) HeaderMap() map[string]string {
	if len(e.Headers) == 0 {
		return nil
	}
	headers := make(map[string]string, len(e.Headers))
	for key, value := range e.Headers {
		headers[key] = value
	}
	return headers
}

// Apply sets the extra request headers on h.
func (e Endpoint) Apply(h http.Header) {
	for key, value := range e.Headers {
		h.Set(key, value)
	}
}

func isReservedHeader(key string) bool {
	for _, reserved := range []string{"Authorization", "Content-Type", "Accept"} {
		if strings.EqualFold(key, reserved) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
