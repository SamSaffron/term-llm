package credentials

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/oauth"
)

var (
	chatGPTCredentialsMu sync.Mutex
	refreshChatGPTToken  = oauth.RefreshToken
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

	creds, err := readChatGPTCredentials(credPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ChatGPT credentials not found (authenticate first)")
		}
		return nil, err
	}
	return creds, nil
}

func readChatGPTCredentials(credPath string) (*ChatGPTCredentials, error) {
	data, err := os.ReadFile(credPath)
	if err != nil {
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
	return withChatGPTCredentialsLock(func(credPath string) error {
		return saveChatGPTCredentials(credPath, creds)
	})
}

func saveChatGPTCredentials(credPath string, creds *ChatGPTCredentials) error {
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	// Write with restrictive permissions (owner read/write only). Use a
	// temp-file-and-rename flow so refresh failures cannot corrupt an existing
	// credentials file.
	if err := writeFileAtomic(credPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write credentials: %w", err)
	}

	return nil
}

// ClearChatGPTCredentials removes the stored ChatGPT credentials.
func ClearChatGPTCredentials() error {
	return withChatGPTCredentialsLock(func(credPath string) error {
		return removeChatGPTCredentials(credPath)
	})
}

// ClearChatGPTCredentialsIfRefreshToken removes credentials only if they still
// contain the refresh token that was rejected. A concurrent refresh or login
// therefore cannot be deleted by a stale provider's authentication retry.
func ClearChatGPTCredentialsIfRefreshToken(refreshToken string) error {
	return withChatGPTCredentialsLock(func(credPath string) error {
		stored, err := readChatGPTCredentials(credPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if stored.RefreshToken != refreshToken {
			return nil
		}
		return removeChatGPTCredentials(credPath)
	})
}

func removeChatGPTCredentials(credPath string) error {
	if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove credentials: %w", err)
	}
	return nil
}

// RefreshChatGPTCredentials refreshes the access token using the refresh token.
// Remote OAuth I/O deliberately runs outside the process and file locks. A
// short compare-and-swap commit prevents a stale refresh from overwriting a
// concurrent login, logout, or refresh.
func RefreshChatGPTCredentials(creds *ChatGPTCredentials) error {
	if creds == nil {
		return fmt.Errorf("missing ChatGPT credentials")
	}
	base := *creds
	hadStored := false
	if err := withChatGPTCredentialsLock(func(credPath string) error {
		stored, err := readChatGPTCredentials(credPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to reload ChatGPT credentials: %w", err)
		}
		if err == nil {
			hadStored = true
			// The CAS baseline must be the generation actually present on disk,
			// not a caller's potentially stale or speculative in-memory copy.
			base = *stored
			if !base.IsExpired() {
				*creds = base
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if !base.IsExpired() {
		*creds = base
		return nil
	}
	if base.RefreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	tokenResp, refreshErr := refreshChatGPTToken(base.RefreshToken)
	return withChatGPTCredentialsLock(func(credPath string) error {
		stored, readErr := readChatGPTCredentials(credPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			return fmt.Errorf("failed to reload ChatGPT credentials: %w", readErr)
		}
		if readErr == nil && !sameChatGPTCredentialGeneration(stored, &base) {
			*creds = *stored
			if !stored.IsExpired() {
				return nil
			}
			// If the credential still names the token generation we exchanged,
			// commit a successful server rotation rather than strand the newly
			// rotated refresh token. A different refresh token is authoritative.
			if refreshErr != nil || tokenResp == nil || tokenResp.RefreshToken == "" || stored.RefreshToken != base.RefreshToken {
				return fmt.Errorf("ChatGPT credentials changed during refresh")
			}
		}
		if os.IsNotExist(readErr) && hadStored {
			return fmt.Errorf("ChatGPT credentials were cleared during refresh")
		}
		if refreshErr != nil {
			*creds = base
			return refreshErr
		}
		replacement := base
		replacement.AccessToken = tokenResp.AccessToken
		replacement.ExpiresAt = time.Now().Unix() + int64(tokenResp.ExpiresIn)
		if tokenResp.RefreshToken != "" {
			replacement.RefreshToken = tokenResp.RefreshToken
		}
		if err := saveChatGPTCredentials(credPath, &replacement); err != nil {
			return fmt.Errorf("failed to save refreshed credentials: %w", err)
		}
		*creds = replacement
		return nil
	})
}

func sameChatGPTCredentialGeneration(a, b *ChatGPTCredentials) bool {
	return a != nil && b != nil &&
		a.AccessToken == b.AccessToken &&
		a.RefreshToken == b.RefreshToken &&
		a.ExpiresAt == b.ExpiresAt &&
		a.AccountID == b.AccountID
}

func withChatGPTCredentialsLock(fn func(credPath string) error) (err error) {
	chatGPTCredentialsMu.Lock()
	defer chatGPTCredentialsMu.Unlock()

	credPath, err := getChatGPTCredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(credPath), 0700); err != nil {
		return fmt.Errorf("failed to create credentials directory: %w", err)
	}

	unlock, err := lockChatGPTCredentials(credPath + ".lock")
	if err != nil {
		return fmt.Errorf("failed to lock ChatGPT credentials: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); err == nil && unlockErr != nil {
			err = fmt.Errorf("failed to unlock ChatGPT credentials: %w", unlockErr)
		}
	}()

	return fn(credPath)
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
