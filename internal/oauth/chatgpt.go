package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	ChatGPTAuthEndpoint  = "https://auth.openai.com/oauth/authorize"
	ChatGPTTokenEndpoint = "https://auth.openai.com/oauth/token"
	// Temporary until we figure out how term-llm can get a proper client ID
	ChatGPTClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	ChatGPTRedirectURI  = "http://localhost:1455/auth/callback"
	ChatGPTScopes       = "openid profile email offline_access"
	ChatGPTCallbackPort = 1455
)

type ChatGPTTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// ChatGPTCredentials holds the OAuth tokens
type ChatGPTCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // Unix timestamp in seconds
	AccountID    string `json:"account_id"` // ChatGPT account ID from JWT
}

// IsExpired returns true if the access token is expired or will expire within 5 minutes
func (c *ChatGPTCredentials) IsExpired() bool {
	// 5-minute buffer before actual expiration
	return time.Now().Unix() > c.ExpiresAt-300
}

// generateCodeVerifier generates a cryptographically random PKCE code verifier
func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// generateCodeChallenge generates the PKCE code challenge from the verifier using S256
func generateCodeChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// generateState generates a random state parameter for CSRF protection
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// buildAuthorizationURL builds the OAuth authorization URL
func buildAuthorizationURL(codeChallenge, state string) string {
	params := url.Values{
		"client_id":                 {ChatGPTClientID},
		"redirect_uri":              {ChatGPTRedirectURI},
		"scope":                     {ChatGPTScopes},
		"code_challenge":            {codeChallenge},
		"code_challenge_method":     {"S256"},
		"response_type":             {"code"},
		"state":                     {state},
		"codex_cli_simplified_flow": {"true"},
		"originator":                {"term-llm"},
	}
	return ChatGPTAuthEndpoint + "?" + params.Encode()
}

// openBrowser opens a URL in the default browser
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return cmd.Start()
}

// exchangeCodeForTokens exchanges the authorization code for tokens
func exchangeCodeForTokens(code, codeVerifier string) (*ChatGPTTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ChatGPTClientID},
		"code":          {code},
		"redirect_uri":  {ChatGPTRedirectURI},
		"code_verifier": {codeVerifier},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", ChatGPTTokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.ErrorDescription != "" {
			return nil, fmt.Errorf("token exchange failed: %s - %s", errResp.Error, errResp.ErrorDescription)
		}
		return nil, fmt.Errorf("token exchange failed: %s", resp.Status)
	}

	var tokenResp ChatGPTTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// RefreshToken refreshes an expired access token
func RefreshToken(refreshToken string) (*ChatGPTTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ChatGPTClientID},
		"refresh_token": {refreshToken},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", ChatGPTTokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Error == "invalid_grant" || strings.Contains(errResp.ErrorDescription, "revoked") {
			return nil, fmt.Errorf("refresh token expired or revoked: please re-authenticate")
		}
		if errResp.ErrorDescription != "" {
			return nil, fmt.Errorf("token refresh failed: %s - %s", errResp.Error, errResp.ErrorDescription)
		}
		return nil, fmt.Errorf("token refresh failed: %s", resp.Status)
	}

	var tokenResp ChatGPTTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// extractAccountIDFromJWT extracts the ChatGPT account ID from a JWT token
func extractAccountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}

	// Decode the payload (middle part)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Try standard base64 with padding
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}

	var claims struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		Auth             *struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
		Organizations []struct {
			ID string `json:"id"`
		} `json:"organizations"`
	}

	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}

	// Check multiple locations for account ID
	if claims.ChatGPTAccountID != "" {
		return claims.ChatGPTAccountID
	}
	if claims.Auth != nil && claims.Auth.ChatGPTAccountID != "" {
		return claims.Auth.ChatGPTAccountID
	}
	if len(claims.Organizations) > 0 {
		return claims.Organizations[0].ID
	}

	return ""
}

// AuthenticateChatGPT runs the full OAuth flow and returns credentials
func AuthenticateChatGPT(ctx context.Context) (*ChatGPTCredentials, error) {
	// Generate PKCE verifier and challenge
	codeVerifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("failed to generate code verifier: %w", err)
	}
	codeChallenge := generateCodeChallenge(codeVerifier)

	// Generate state for CSRF protection
	state, err := generateState()
	if err != nil {
		return nil, fmt.Errorf("failed to generate state: %w", err)
	}

	// Build authorization URL
	authURL := buildAuthorizationURL(codeChallenge, state)

	// Start local callback server
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", ChatGPTCallbackPort))
	if err != nil {
		return nil, fmt.Errorf("failed to start callback server on port %d: %w", ChatGPTCallbackPort, err)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/auth/callback" {
				http.NotFound(w, r)
				return
			}

			// Validate state
			if r.URL.Query().Get("state") != state {
				errChan <- fmt.Errorf("state mismatch: possible CSRF attack")
				http.Error(w, "Invalid state parameter", http.StatusBadRequest)
				return
			}

			// Check for error
			if errMsg := r.URL.Query().Get("error"); errMsg != "" {
				errDesc := r.URL.Query().Get("error_description")
				errChan <- fmt.Errorf("OAuth error: %s - %s", errMsg, errDesc)
				http.Error(w, errDesc, http.StatusBadRequest)
				return
			}

			// Get authorization code
			code := r.URL.Query().Get("code")
			if code == "" {
				errChan <- fmt.Errorf("no authorization code received")
				http.Error(w, "No code received", http.StatusBadRequest)
				return
			}

			// Send success response
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Authentication Successful</title></head>
<body style="font-family: system-ui; text-align: center; padding: 50px;">
<h1>Authentication Successful!</h1>
<p>You can close this window and return to the terminal.</p>
</body>
</html>`))

			codeChan <- code
		}),
	}

	// Start server in goroutine
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("callback server error: %w", err)
		}
	}()

	defer server.Shutdown(context.Background())

	// Open browser
	if err := openBrowser(authURL); err != nil {
		fmt.Printf("Could not open browser. Please visit this URL:\n%s\n", authURL)
	}

	// Wait for callback or timeout
	select {
	case code := <-codeChan:
		// Exchange code for tokens
		tokenResp, err := exchangeCodeForTokens(code, codeVerifier)
		if err != nil {
			return nil, err
		}

		// Extract account ID from JWT
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

	case err := <-errChan:
		return nil, err

	case <-ctx.Done():
		return nil, ctx.Err()

	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("authentication timed out after 5 minutes")
	}
}
