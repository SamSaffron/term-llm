package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The device code flow talks to OpenAI's /deviceauth endpoints. It is
// the same flow codex uses for headless/remote sign-in — no local
// callback server, no browser redirect to localhost, just a URL + code
// the user visits on any device.
const (
	ChatGPTDeviceAuthBaseURL     = "https://auth.openai.com/api/accounts"
	ChatGPTDeviceVerificationURL = "https://auth.openai.com/codex/device"
	ChatGPTDeviceRedirectURI     = "https://auth.openai.com/deviceauth/callback"
)

// ErrChatGPTDeviceCodeNotEnabled is returned when the backend does not
// advertise device-code login (HTTP 404). Callers should fall back to
// the interactive browser flow.
var ErrChatGPTDeviceCodeNotEnabled = errors.New("chatgpt device code login is not enabled")

// ChatGPTDeviceCode is the user-facing output of the usercode request.
type ChatGPTDeviceCode struct {
	VerificationURL string
	UserCode        string

	deviceAuthID string
	interval     time.Duration
}

type deviceUserCodeResp struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	// The server has been observed returning interval as a string in
	// some deployments — accept either representation.
	Interval json.RawMessage `json:"interval"`
}

type deviceCodeSuccessResp struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

// RequestChatGPTDeviceCode asks OpenAI for a new device-auth pair.
// Returns ErrChatGPTDeviceCodeNotEnabled when the server returns 404.
func RequestChatGPTDeviceCode(ctx context.Context) (*ChatGPTDeviceCode, error) {
	body, err := json.Marshal(map[string]string{"client_id": ChatGPTClientID})
	if err != nil {
		return nil, fmt.Errorf("encode usercode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		ChatGPTDeviceAuthBaseURL+"/deviceauth/usercode",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build usercode request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usercode request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrChatGPTDeviceCodeNotEnabled
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usercode request returned status %s", resp.Status)
	}

	var parsed deviceUserCodeResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode usercode response: %w", err)
	}
	if parsed.DeviceAuthID == "" || parsed.UserCode == "" {
		return nil, fmt.Errorf("usercode response missing required fields")
	}

	interval, err := parseDeviceInterval(parsed.Interval)
	if err != nil {
		return nil, err
	}

	return &ChatGPTDeviceCode{
		VerificationURL: ChatGPTDeviceVerificationURL,
		UserCode:        parsed.UserCode,
		deviceAuthID:    parsed.DeviceAuthID,
		interval:        interval,
	}, nil
}

// parseDeviceInterval decodes the usercode "interval" field, which the
// server may return as a JSON number or a JSON string. Falls back to 5s
// when unset or zero — matches codex's behavior.
func parseDeviceInterval(raw json.RawMessage) (time.Duration, error) {
	const fallback = 5 * time.Second
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return fallback, nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, fmt.Errorf("decode interval string: %w", err)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return fallback, nil
		}
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("parse interval string: %w", err)
		}
		if n == 0 {
			return fallback, nil
		}
		return time.Duration(n) * time.Second, nil
	}
	var n uint64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("decode interval number: %w", err)
	}
	if n == 0 {
		return fallback, nil
	}
	return time.Duration(n) * time.Second, nil
}

// pollForDeviceToken blocks until the user approves the code or ctx is
// cancelled / the device code expires (server-side, ~15 minutes).
func pollForDeviceToken(ctx context.Context, dc *ChatGPTDeviceCode) (*deviceCodeSuccessResp, error) {
	body, err := json.Marshal(map[string]string{
		"device_auth_id": dc.deviceAuthID,
		"user_code":      dc.UserCode,
	})
	if err != nil {
		return nil, fmt.Errorf("encode token poll request: %w", err)
	}

	for {
		req, err := http.NewRequestWithContext(ctx, "POST",
			ChatGPTDeviceAuthBaseURL+"/deviceauth/token",
			strings.NewReader(string(body)))
		if err != nil {
			return nil, fmt.Errorf("build token poll request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := oauthHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("token poll request failed: %w", err)
		}

		switch resp.StatusCode {
		case http.StatusOK:
			var parsed deviceCodeSuccessResp
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("decode token poll response: %w", err)
			}
			resp.Body.Close()
			return &parsed, nil

		case http.StatusForbidden, http.StatusNotFound:
			// Still waiting for user approval.
			resp.Body.Close()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(dc.interval):
			}

		default:
			status := resp.Status
			resp.Body.Close()
			return nil, fmt.Errorf("device auth poll failed: %s", status)
		}
	}
}

// exchangeDeviceCodeForTokens performs the final /oauth/token call,
// using the authorization_code + code_verifier returned by the poll
// endpoint. The redirect_uri must match what the auth server expects
// for device flow (auth.openai.com/deviceauth/callback), not the
// localhost URI used by the browser flow.
func exchangeDeviceCodeForTokens(ctx context.Context, resp *deviceCodeSuccessResp) (*ChatGPTTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ChatGPTClientID},
		"code":          {resp.AuthorizationCode},
		"redirect_uri":  {ChatGPTDeviceRedirectURI},
		"code_verifier": {resp.CodeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", ChatGPTTokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build device token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpResp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device token exchange request failed: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		json.NewDecoder(httpResp.Body).Decode(&errResp)
		if errResp.ErrorDescription != "" {
			return nil, fmt.Errorf("device token exchange failed: %s - %s", errResp.Error, errResp.ErrorDescription)
		}
		return nil, fmt.Errorf("device token exchange failed: %s", httpResp.Status)
	}

	var tokenResp ChatGPTTokenResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode device token response: %w", err)
	}
	return &tokenResp, nil
}

// AuthenticateChatGPTDevice runs the full device-code OAuth flow. The
// caller is responsible for printing the verification URL + user code
// between RequestChatGPTDeviceCode and this call — taking a callback
// here keeps the prompt UX out of the oauth package.
func AuthenticateChatGPTDevice(ctx context.Context, dc *ChatGPTDeviceCode) (*ChatGPTCredentials, error) {
	codeResp, err := pollForDeviceToken(ctx, dc)
	if err != nil {
		return nil, err
	}

	tokenResp, err := exchangeDeviceCodeForTokens(ctx, codeResp)
	if err != nil {
		return nil, err
	}

	accountID := ""
	if tokenResp.IDToken != "" {
		accountID = extractAccountIDFromJWT(tokenResp.IDToken)
	}
	if accountID == "" && tokenResp.AccessToken != "" {
		accountID = extractAccountIDFromJWT(tokenResp.AccessToken)
	}

	return &ChatGPTCredentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Unix() + int64(tokenResp.ExpiresIn),
		AccountID:    accountID,
	}, nil
}
