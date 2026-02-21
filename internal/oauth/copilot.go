package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var oauthHTTPClient = &http.Client{Timeout: 30 * time.Second}

const (
	// CopilotClientID is the VS Code GitHub Copilot extension's client ID.
	// This is required for access to GitHub's internal Copilot APIs (usage, token exchange).
	// Using the VS Code client ID is a common approach in the Copilot developer community.
	CopilotClientID = "Iv1.b507a08c87ecfe98"

	// CopilotScope is the OAuth scope required for Copilot API access
	CopilotScope = "read:user"

	// GitHub device code OAuth endpoints
	githubDeviceCodeURL = "https://github.com/login/device/code"
	githubTokenURL      = "https://github.com/login/oauth/access_token"
)

// CopilotDeviceCodeResponse holds the device code response from GitHub
type CopilotDeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"` // Polling interval in seconds
}

// CopilotTokenResponse holds the token response from GitHub
type CopilotTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error,omitempty"`
}

// CopilotCredentials holds the OAuth token for Copilot.
// Note: credentials.CopilotCredentials is the storage type with the same fields.
type CopilotCredentials struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"` // 0 = no expiry tracking
}

// RequestCopilotDeviceCode initiates the device code flow
func RequestCopilotDeviceCode() (*CopilotDeviceCodeResponse, error) {
	data := url.Values{
		"client_id": {CopilotClientID},
		"scope":     {CopilotScope},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", githubDeviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request failed: %s", resp.Status)
	}

	var deviceResp CopilotDeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&deviceResp); err != nil {
		return nil, fmt.Errorf("failed to decode device code response: %w", err)
	}

	return &deviceResp, nil
}

// PollForCopilotToken polls for the access token after the user has authorized
func PollForCopilotToken(ctx context.Context, deviceCode string, interval int) (*CopilotTokenResponse, error) {
	// Add 3 seconds margin as recommended by RFC 8628
	pollInterval := time.Duration(interval+3) * time.Second

	data := url.Values{
		"client_id":   {CopilotClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
			// Continue polling
		}

		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, "POST", githubTokenURL, strings.NewReader(data.Encode()))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		resp, err := oauthHTTPClient.Do(req)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("token request failed: %w", err)
		}

		var tokenResp CopilotTokenResponse
		err = json.NewDecoder(resp.Body).Decode(&tokenResp)
		resp.Body.Close()
		cancel()

		if err != nil {
			return nil, fmt.Errorf("failed to decode token response: %w", err)
		}

		switch tokenResp.Error {
		case "":
			// Success - we have a token
			return &tokenResp, nil
		case "authorization_pending":
			// User hasn't authorized yet, keep polling
			continue
		case "slow_down":
			// Add 5 seconds to interval as per RFC 8628
			pollInterval += 5 * time.Second
			continue
		case "expired_token":
			return nil, fmt.Errorf("device code expired - please try again")
		case "access_denied":
			return nil, fmt.Errorf("authorization denied by user")
		default:
			return nil, fmt.Errorf("OAuth error: %s", tokenResp.Error)
		}
	}
}

// AuthenticateCopilot runs the full device code OAuth flow and returns credentials
func AuthenticateCopilot(ctx context.Context) (*CopilotCredentials, error) {
	// Request device code
	deviceResp, err := RequestCopilotDeviceCode()
	if err != nil {
		return nil, err
	}

	// Display instructions to user
	fmt.Printf("\nTo authenticate with GitHub Copilot:\n")
	fmt.Printf("  1. Visit: %s\n", deviceResp.VerificationURI)
	fmt.Printf("  2. Enter code: %s\n\n", deviceResp.UserCode)
	fmt.Printf("Waiting for authorization...")

	// Open browser automatically
	if err := openBrowser(deviceResp.VerificationURI); err != nil {
		// Not fatal - user can manually visit the URL
	}

	// Poll for token with timeout
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(deviceResp.ExpiresIn)*time.Second)
	defer cancel()

	tokenResp, err := PollForCopilotToken(pollCtx, deviceResp.DeviceCode, deviceResp.Interval)
	if err != nil {
		return nil, err
	}

	fmt.Println(" done!")

	return &CopilotCredentials{
		AccessToken: tokenResp.AccessToken,
		ExpiresAt:   0, // GitHub tokens don't have a standard expiry
	}, nil
}
