package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func stubCopilotSessionRefresh(t *testing.T, apiBaseURL string) *int {
	t.Helper()

	origClient := copilotHTTPClient
	t.Cleanup(func() {
		copilotHTTPClient = origClient
	})

	requests := 0
	copilotHTTPClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			if got := r.Header.Get("Authorization"); got != "token oauth-token" {
				t.Fatalf("Authorization header = %q, want GitHub OAuth token", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
					`{"token":"fresh-token","expires_at":%d,"refresh_in":1500,"endpoints":{"api":%q}}`,
					time.Now().Add(25*time.Minute).Unix(), apiBaseURL,
				))),
			}, nil
		}),
	}
	return &requests
}

func TestEnsureValidSessionKeepsUnexpiredToken(t *testing.T) {
	requests := stubCopilotSessionRefresh(t, "https://api.githubcopilot.com")

	provider := &CopilotProvider{
		creds:              &credentials.CopilotCredentials{AccessToken: "oauth-token"},
		sessionToken:       "existing-token",
		sessionTokenExpiry: time.Now().Add(10 * time.Minute),
	}

	if err := provider.ensureValidSession(context.Background()); err != nil {
		t.Fatalf("ensureValidSession returned error: %v", err)
	}
	if *requests != 0 {
		t.Fatalf("expected no token refresh request, got %d", *requests)
	}
	if provider.sessionToken != "existing-token" {
		t.Fatalf("expected existing session token to be kept, got %q", provider.sessionToken)
	}
}

func TestEnsureValidSessionRefreshesNearExpiry(t *testing.T) {
	requests := stubCopilotSessionRefresh(t, "https://api.business.githubcopilot.com")

	provider := &CopilotProvider{
		creds:              &credentials.CopilotCredentials{AccessToken: "oauth-token"},
		sessionToken:       "stale-token",
		sessionTokenExpiry: time.Now().Add(30 * time.Second),
	}

	if err := provider.ensureValidSession(context.Background()); err != nil {
		t.Fatalf("ensureValidSession returned error: %v", err)
	}
	if *requests != 1 {
		t.Fatalf("expected 1 token refresh request, got %d", *requests)
	}
	if provider.sessionToken != "fresh-token" {
		t.Fatalf("expected session token to be refreshed, got %q", provider.sessionToken)
	}
	if provider.apiBaseURL != "https://api.business.githubcopilot.com" {
		t.Fatalf("expected api base URL to be updated from token response, got %q", provider.apiBaseURL)
	}
}

func TestEnsureValidSessionRefreshesMissingExpiry(t *testing.T) {
	requests := stubCopilotSessionRefresh(t, "https://api.githubcopilot.com")

	provider := &CopilotProvider{
		creds:        &credentials.CopilotCredentials{AccessToken: "oauth-token"},
		sessionToken: "token-without-expiry",
	}

	if err := provider.ensureValidSession(context.Background()); err != nil {
		t.Fatalf("ensureValidSession returned error: %v", err)
	}
	if *requests != 1 {
		t.Fatalf("expected 1 token refresh request, got %d", *requests)
	}
	if provider.sessionToken != "fresh-token" {
		t.Fatalf("expected session token to be refreshed, got %q", provider.sessionToken)
	}
}
