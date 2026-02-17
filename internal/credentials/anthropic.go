package credentials

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// AnthropicOAuthCredentials holds the OAuth token for Anthropic API access.
// Generated via `claude setup-token` (requires Claude subscription).
type AnthropicOAuthCredentials struct {
	AccessToken string `json:"access_token"`
}

// getAnthropicOAuthCredentialsPath returns the path to the Anthropic OAuth credentials file
func getAnthropicOAuthCredentialsPath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "term-llm", "anthropic_oauth.json"), nil
}

// GetAnthropicOAuthCredentials retrieves the Anthropic OAuth credentials from storage.
// Returns an error if credentials don't exist or are invalid.
func GetAnthropicOAuthCredentials() (*AnthropicOAuthCredentials, error) {
	credPath, err := getAnthropicOAuthCredentialsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(credPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Anthropic OAuth credentials not found (run 'claude setup-token' first)")
		}
		return nil, fmt.Errorf("failed to read credentials: %w", err)
	}

	var creds AnthropicOAuthCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("failed to parse credentials: %w", err)
	}

	if creds.AccessToken == "" {
		return nil, fmt.Errorf("invalid credentials: missing access token")
	}

	return &creds, nil
}

// SaveAnthropicOAuthCredentials saves Anthropic OAuth credentials to storage.
func SaveAnthropicOAuthCredentials(creds *AnthropicOAuthCredentials) error {
	credPath, err := getAnthropicOAuthCredentialsPath()
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

// ClearAnthropicOAuthCredentials removes the stored Anthropic OAuth credentials.
func ClearAnthropicOAuthCredentials() error {
	credPath, err := getAnthropicOAuthCredentialsPath()
	if err != nil {
		return err
	}

	if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove credentials: %w", err)
	}

	return nil
}

// AnthropicOAuthCredentialsExist returns true if Anthropic OAuth credentials are stored.
func AnthropicOAuthCredentialsExist() bool {
	credPath, err := getAnthropicOAuthCredentialsPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(credPath)
	return err == nil
}
