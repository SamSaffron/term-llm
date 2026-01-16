package credentials

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samsaffron/term-llm/internal/oauth"
)

// ChatGPTCredentials holds the OAuth tokens for ChatGPT
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

// getChatGPTCredentialsPath returns the path to the ChatGPT credentials file
func getChatGPTCredentialsPath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "term-llm", "chatgpt_oauth.json"), nil
}

// GetChatGPTCredentials retrieves the ChatGPT OAuth credentials from storage.
// Returns an error if credentials don't exist or are invalid.
func GetChatGPTCredentials() (*ChatGPTCredentials, error) {
	credPath, err := getChatGPTCredentialsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(credPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ChatGPT credentials not found (authenticate first)")
		}
		return nil, fmt.Errorf("failed to read credentials: %w", err)
	}

	var creds ChatGPTCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("failed to parse credentials: %w", err)
	}

	if creds.AccessToken == "" {
		return nil, fmt.Errorf("invalid credentials: missing access token")
	}

	return &creds, nil
}

// SaveChatGPTCredentials saves ChatGPT OAuth credentials to storage.
func SaveChatGPTCredentials(creds *ChatGPTCredentials) error {
	credPath, err := getChatGPTCredentialsPath()
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(credPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create credentials directory: %w", err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	// Write with restrictive permissions (owner read/write only)
	if err := os.WriteFile(credPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write credentials: %w", err)
	}

	return nil
}

// ClearChatGPTCredentials removes the stored ChatGPT credentials.
func ClearChatGPTCredentials() error {
	credPath, err := getChatGPTCredentialsPath()
	if err != nil {
		return err
	}

	if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove credentials: %w", err)
	}

	return nil
}

// RefreshChatGPTCredentials refreshes the access token using the refresh token.
// The updated credentials are automatically saved to storage.
func RefreshChatGPTCredentials(creds *ChatGPTCredentials) error {
	if creds.RefreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	tokenResp, err := oauth.RefreshToken(creds.RefreshToken)
	if err != nil {
		return err
	}

	// Update credentials
	creds.AccessToken = tokenResp.AccessToken
	creds.ExpiresAt = time.Now().Unix() + int64(tokenResp.ExpiresIn)

	// Update refresh token if a new one was provided
	if tokenResp.RefreshToken != "" {
		creds.RefreshToken = tokenResp.RefreshToken
	}

	// Save updated credentials
	if err := SaveChatGPTCredentials(creds); err != nil {
		return fmt.Errorf("failed to save refreshed credentials: %w", err)
	}

	return nil
}

// ChatGPTCredentialsExist returns true if ChatGPT credentials are stored.
func ChatGPTCredentialsExist() bool {
	credPath, err := getChatGPTCredentialsPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(credPath)
	return err == nil
}
