package credentials

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CopilotCredentials holds the OAuth token for GitHub Copilot
type CopilotCredentials struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"` // Unix timestamp in seconds, 0 = no expiry
}

// IsExpired returns true if the access token is expired or will expire within 5 minutes.
// GitHub OAuth tokens are typically long-lived, so this usually returns false.
func (c *CopilotCredentials) IsExpired() bool {
	if c.ExpiresAt == 0 {
		return false // No expiry set
	}
	// 5-minute buffer before actual expiration
	return time.Now().Unix() > c.ExpiresAt-300
}

// getCopilotCredentialsPath returns the path to the Copilot credentials file
func getCopilotCredentialsPath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "term-llm", "copilot_oauth.json"), nil
}

// GetCopilotCredentials retrieves the Copilot OAuth credentials from storage.
// Returns an error if credentials don't exist or are invalid.
func GetCopilotCredentials() (*CopilotCredentials, error) {
	credPath, err := getCopilotCredentialsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(credPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Copilot credentials not found (authenticate first)")
		}
		return nil, fmt.Errorf("failed to read credentials: %w", err)
	}

	var creds CopilotCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("failed to parse credentials: %w", err)
	}

	if creds.AccessToken == "" {
		return nil, fmt.Errorf("invalid credentials: missing access token")
	}

	return &creds, nil
}

// SaveCopilotCredentials saves Copilot OAuth credentials to storage.
func SaveCopilotCredentials(creds *CopilotCredentials) error {
	credPath, err := getCopilotCredentialsPath()
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

// ClearCopilotCredentials removes the stored Copilot credentials.
func ClearCopilotCredentials() error {
	credPath, err := getCopilotCredentialsPath()
	if err != nil {
		return err
	}

	if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove credentials: %w", err)
	}

	return nil
}

// CopilotCredentialsExist returns true if Copilot credentials are stored.
func CopilotCredentialsExist() bool {
	credPath, err := getCopilotCredentialsPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(credPath)
	return err == nil
}
