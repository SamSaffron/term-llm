package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

func TestProviderErrorsAreStructuredActionableAndSafe(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		providerType config.ProviderType
		wantStatus   int
		wantCode     string
		wantAction   string
	}{
		{"api key", providerhttp.NewStatusErrorString("OpenAI", 401, "401 Unauthorized", nil, `raw-body api_key=super-secret /srv/private`), config.ProviderTypeOpenAI, 401, "provider_api_key_unauthenticated", "update the API key"},
		{"missing API key", errors.New("OPENAI_API_KEY is required at /srv/private"), config.ProviderTypeOpenAI, 401, "provider_api_key_unauthenticated", "update the API key"},
		{"oauth", providerhttp.NewStatusErrorString("ChatGPT", 401, "401 Unauthorized", nil, `oauth_token=super-secret`), config.ProviderTypeChatGPT, 401, "provider_oauth_unauthenticated", "gateway host"},
		{"missing oauth", errors.New("OAuth login credential missing at /srv/private"), config.ProviderTypeCopilot, 401, "provider_oauth_unauthenticated", "gateway host"},
		{"rate limit", providerhttp.NewStatusErrorString("OpenAI", 429, "429 Too Many Requests", nil, `account secret limit`), config.ProviderTypeOpenAI, 429, "provider_rate_limited", "wait and retry"},
		{"typed rate limit", &llm.RateLimitError{Message: "rate limit secret"}, config.ProviderTypeOpenAI, 429, "provider_rate_limited", "wait and retry"},
		{"context", errors.New("maximum context length exceeded; prompt /tmp/private"), config.ProviderTypeAnthropic, 400, "provider_context_limit", "compact"},
		{"model", providerhttp.NewStatusErrorString("OpenAI", 400, "400 Bad Request", nil, `model private-preview does not exist for key secret`), config.ProviderTypeOpenAI, 400, "provider_model_invalid", "catalog"},
		{"upstream", providerhttp.NewStatusErrorString("OpenAI", 503, "503 Service Unavailable", nil, `upstream raw body /etc/passwd`), config.ProviderTypeOpenAI, 502, "provider_upstream_failure", "diagnostics"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, code := classifyProviderError(tc.err, string(tc.providerType))
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("classification = %d/%s, want %d/%s", status, code, tc.wantStatus, tc.wantCode)
			}
			message := safeProviderErrorMessage(code, "remote")
			if !strings.Contains(message, "gateway provider") || !strings.Contains(message, tc.wantAction) {
				t.Fatalf("safe message is not actionable: %q", message)
			}
			for _, forbidden := range []string{"super-secret", "raw-body", "/srv/", "/tmp/", "/etc/", "private-preview"} {
				if strings.Contains(message, forbidden) {
					t.Fatalf("safe message leaked %q: %q", forbidden, message)
				}
			}
		})
	}
}

func TestCancellationErrorIsClassifiedExplicitly(t *testing.T) {
	status, code := classifyProviderError(fmt.Errorf("stream stopped: %w", context.Canceled), string(config.ProviderTypeOpenAI))
	if status != 499 || code != "canceled" {
		t.Fatalf("cancellation classification = %d/%s", status, code)
	}
	if message := safeProviderErrorMessage(code, "openai"); !strings.Contains(message, "canceled") || strings.Contains(message, "upstream") {
		t.Fatalf("cancellation message = %q", message)
	}
}

func TestSafeErrorFallbackIsGatewaySpecific(t *testing.T) {
	status, code := classifyProviderError(errors.New("opaque transport failure"), string(config.ProviderTypeOpenAI))
	if status != http.StatusBadGateway || code != "provider_upstream_failure" {
		t.Fatalf("fallback = %d/%s", status, code)
	}
	if got := safeProviderErrorMessage(code, "openai"); !strings.Contains(got, "gateway provider \"openai\"") || !strings.Contains(got, "retry") {
		t.Fatalf("fallback message = %q", got)
	}
}
