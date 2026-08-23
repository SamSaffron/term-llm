package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/oauth"
)

const (
	grokCredentialsFile    = "grok_oauth.json"
	maxGrokCredentialBytes = 64 * 1024
	grokRefreshSkew        = 60 * time.Second
)

var (
	grokCredentialsMu sync.Mutex
	grokOAuthClient   = oauth.NewGrokOAuthClient(nil)
)

type GrokOAuthClient interface {
	RefreshToken(context.Context, string) (*oauth.GrokTokenResponse, error)
	UserInfo(context.Context, string) (string, error)
}

type GrokCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	AccountID    string `json:"account_id"`
}

func (c *GrokCredentials) IsExpired() bool {
	return c == nil || time.Now().Unix() >= c.ExpiresAt-int64(grokRefreshSkew/time.Second)
}

func getGrokCredentialsPath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("get home directory: %w", err)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "term-llm", grokCredentialsFile), nil
}

func GetGrokCredentials() (*GrokCredentials, error) {
	path, err := getGrokCredentialsPath()
	if err != nil {
		return nil, err
	}
	creds, err := readGrokCredentials(path)
	if os.IsNotExist(err) {
		return nil, errors.New("Grok credentials not found; run 'term-llm auth login grok'")
	}
	return creds, err
}

func readGrokCredentials(path string) (*GrokCredentials, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("Grok credentials file is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("Grok credentials file has insecure permissions %04o; require 0600", info.Mode().Perm())
	}
	if info.Size() > maxGrokCredentialBytes {
		return nil, errors.New("Grok credentials file exceeds size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Grok credentials: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxGrokCredentialBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Grok credentials: %w", err)
	}
	if len(data) > maxGrokCredentialBytes {
		return nil, errors.New("Grok credentials file exceeds size limit")
	}
	var creds GrokCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse Grok credentials: %w", err)
	}
	if err := validateGrokCredentials(&creds); err != nil {
		return nil, err
	}
	return &creds, nil
}

func validateGrokCredentials(creds *GrokCredentials) error {
	if creds == nil || creds.AccessToken == "" || creds.RefreshToken == "" || creds.ExpiresAt <= 0 {
		return errors.New("invalid Grok credentials: missing required field")
	}
	if !oauth.ValidGrokAccountID(creds.AccountID) {
		return errors.New("invalid Grok credentials: unsafe account ID")
	}
	return nil
}

func SaveGrokCredentials(creds *GrokCredentials) error {
	if err := validateGrokCredentials(creds); err != nil {
		return err
	}
	return withGrokCredentialsLock(func(path string) error {
		return saveGrokCredentials(path, creds)
	})
}

func saveGrokCredentials(path string, creds *GrokCredentials) error {
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal Grok credentials: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write Grok credentials: %w", err)
	}
	return nil
}

func ClearGrokCredentials() error {
	return withGrokCredentialsLock(func(path string) error { return removeGrokCredentials(path) })
}

func ClearGrokCredentialsIfRefreshToken(refreshToken string) error {
	return withGrokCredentialsLock(func(path string) error {
		stored, err := readGrokCredentials(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if stored.RefreshToken != refreshToken {
			return nil
		}
		return removeGrokCredentials(path)
	})
}

func removeGrokCredentials(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Grok credentials: %w", err)
	}
	return nil
}

func RefreshGrokCredentials(ctx context.Context, creds *GrokCredentials, force bool) error {
	return RefreshGrokCredentialsWithClient(ctx, creds, force, grokOAuthClient)
}

func RefreshGrokCredentialsWithClient(ctx context.Context, creds *GrokCredentials, force bool, client GrokOAuthClient) error {
	if creds == nil {
		return errors.New("missing Grok credentials")
	}
	if client == nil {
		return errors.New("missing Grok OAuth client")
	}
	requestedAccess := creds.AccessToken
	requestedRefresh := creds.RefreshToken
	return withGrokCredentialsLock(func(path string) error {
		stored, err := readGrokCredentials(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("reload Grok credentials: %w", err)
		}
		if err == nil {
			accessChanged := stored.AccessToken != requestedAccess
			if stored.RefreshToken != requestedRefresh || accessChanged || stored.ExpiresAt != creds.ExpiresAt {
				*creds = *stored
			}
			// During a forced 401 recovery, only a genuinely different access
			// token proves that another process already replaced the failed token.
			if force && accessChanged {
				return nil
			}
			if !force && !creds.IsExpired() {
				return nil
			}
		}
		if !force && !creds.IsExpired() {
			return nil
		}
		if creds.RefreshToken == "" {
			return errors.New("no Grok refresh token available")
		}
		token, err := client.RefreshToken(ctx, creds.RefreshToken)
		if err != nil {
			return err
		}
		refreshToken := creds.RefreshToken
		if token.RefreshToken != "" {
			refreshToken = token.RefreshToken
		}
		expiresAt, err := oauth.GrokExpiryUnix(time.Now().Unix(), token.ExpiresIn)
		if err != nil {
			return err
		}
		accountID, err := client.UserInfo(ctx, token.AccessToken)
		if err != nil {
			if refreshToken != creds.RefreshToken {
				// Refresh-token rotation may invalidate the token currently on disk.
				// Persist the rotated token with an already-expired access generation so
				// the next attempt must refresh and re-verify account continuity before
				// using it. This is the first phase of a two-phase verified refresh.
				recovery := &GrokCredentials{
					AccessToken:  creds.AccessToken,
					RefreshToken: refreshToken,
					ExpiresAt:    time.Now().Unix(),
					AccountID:    creds.AccountID,
				}
				if saveErr := saveGrokCredentials(path, recovery); saveErr != nil {
					return fmt.Errorf("verify refreshed Grok account: %v; preserve rotated refresh token: %w", err, saveErr)
				}
				*creds = *recovery
				return fmt.Errorf("verify refreshed Grok account: %w (rotated refresh token was preserved for retry)", err)
			}
			return err
		}
		if accountID != creds.AccountID {
			if clearErr := removeGrokCredentials(path); clearErr != nil {
				return fmt.Errorf("refreshed Grok session belongs to a different account; clear stale credentials: %w", clearErr)
			}
			return errors.New("refreshed Grok session belongs to a different account; local credentials were cleared; run 'term-llm auth login grok'")
		}
		replacement := &GrokCredentials{
			AccessToken:  token.AccessToken,
			RefreshToken: refreshToken,
			ExpiresAt:    expiresAt,
			AccountID:    accountID,
		}
		if err := validateGrokCredentials(replacement); err != nil {
			return err
		}
		if err := saveGrokCredentials(path, replacement); err != nil {
			return fmt.Errorf("save refreshed Grok credentials: %w", err)
		}
		*creds = *replacement
		return nil
	})
}

func withGrokCredentialsLock(fn func(path string) error) (err error) {
	grokCredentialsMu.Lock()
	defer grokCredentialsMu.Unlock()
	path, err := getGrokCredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Grok credentials directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("secure Grok credentials directory: %w", err)
	}
	unlock, err := filelock.Lock(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock Grok credentials: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); err == nil && unlockErr != nil {
			err = fmt.Errorf("unlock Grok credentials: %w", unlockErr)
		}
	}()
	return fn(path)
}

func GrokCredentialsExist() bool {
	path, err := getGrokCredentialsPath()
	if err != nil {
		return false
	}
	_, err = os.Lstat(path)
	return err == nil
}
