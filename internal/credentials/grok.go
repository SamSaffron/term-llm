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

	requested := *creds
	base := requested
	hadStored := false
	needsRefresh := true
	if err := withGrokCredentialsLock(func(path string) error {
		stored, err := readGrokCredentials(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("reload Grok credentials: %w", err)
		}
		if err == nil {
			hadStored = true
			accessChanged := stored.AccessToken != requested.AccessToken
			// Refresh and commit against the complete generation actually on disk,
			// including account identity.
			base = *stored
			if (force && accessChanged) || (!force && !base.IsExpired()) {
				needsRefresh = false
				*creds = base
			}
		} else if !force && !base.IsExpired() {
			needsRefresh = false
		}
		return nil
	}); err != nil {
		return err
	}
	if !needsRefresh {
		return nil
	}
	if base.RefreshToken == "" {
		return errors.New("no Grok refresh token available")
	}

	// Token exchange is provider I/O, not metadata work; never hold the shared
	// credential lock while it is in flight.
	token, refreshErr := client.RefreshToken(ctx, base.RefreshToken)
	if refreshErr != nil {
		return reconcileGrokRefreshFailure(creds, &base, requested.AccessToken, force, refreshErr)
	}
	refreshToken := base.RefreshToken
	if token.RefreshToken != "" {
		refreshToken = token.RefreshToken
	}
	expiresAt, err := oauth.GrokExpiryUnix(time.Now().Unix(), token.ExpiresIn)
	if err != nil {
		return err
	}

	// Persist a rotated refresh token immediately, before remote account
	// verification, so a transient verification failure cannot strand it.
	recovery := &GrokCredentials{
		AccessToken:  base.AccessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Unix(),
		AccountID:    base.AccountID,
	}
	checkpointed := false
	if err := withGrokCredentialsLock(func(path string) error {
		stored, readErr := readGrokCredentials(path)
		if readErr != nil && !os.IsNotExist(readErr) {
			return fmt.Errorf("reload Grok credentials: %w", readErr)
		}
		if readErr == nil && !sameGrokCredentialGeneration(stored, &base) {
			// Another independent refresh may already have checkpointed the same
			// rotation. Join only at the durable checkpoint; account verification
			// still proceeds independently and no caller waits for its lifecycle.
			if stored.AccessToken == base.AccessToken && stored.RefreshToken == refreshToken && stored.AccountID == base.AccountID && stored.IsExpired() {
				recovery = stored
				checkpointed = true
				return nil
			}
			*creds = *stored
			if grokGenerationSatisfiesRefresh(stored, requested.AccessToken, force) {
				return nil
			}
			return errors.New("Grok credentials changed during refresh but still require refresh")
		}
		if os.IsNotExist(readErr) && hadStored {
			return errors.New("Grok credentials were cleared during refresh")
		}
		if err := saveGrokCredentials(path, recovery); err != nil {
			return fmt.Errorf("preserve rotated Grok refresh token: %w", err)
		}
		checkpointed = true
		return nil
	}); err != nil {
		return err
	}
	if !checkpointed {
		return nil
	}

	accountID, verifyErr := client.UserInfo(ctx, token.AccessToken)
	return withGrokCredentialsLock(func(path string) error {
		stored, readErr := readGrokCredentials(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return errors.New("Grok credentials were cleared during account verification")
			}
			return fmt.Errorf("reload Grok credentials: %w", readErr)
		}
		if !sameGrokCredentialGeneration(stored, recovery) {
			*creds = *stored
			if grokGenerationSatisfiesRefresh(stored, requested.AccessToken, force) {
				return nil
			}
			return errors.New("Grok credentials changed during account verification but still require refresh")
		}
		if verifyErr != nil {
			*creds = *recovery
			return fmt.Errorf("verify refreshed Grok account: %w (rotated refresh token was preserved for retry)", verifyErr)
		}
		if accountID != base.AccountID {
			if clearErr := removeGrokCredentials(path); clearErr != nil {
				return fmt.Errorf("refreshed Grok session belongs to a different account; clear stale credentials: %w", clearErr)
			}
			return errors.New("refreshed Grok session belongs to a different account; local credentials were cleared; run 'term-llm auth login grok'")
		}
		replacement := &GrokCredentials{AccessToken: token.AccessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt, AccountID: accountID}
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

func sameGrokCredentialGeneration(a, b *GrokCredentials) bool {
	return a != nil && b != nil &&
		a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken &&
		a.ExpiresAt == b.ExpiresAt && a.AccountID == b.AccountID
}

func grokGenerationSatisfiesRefresh(stored *GrokCredentials, rejectedAccess string, force bool) bool {
	return stored != nil && ((force && stored.AccessToken != rejectedAccess) || (!force && !stored.IsExpired()))
}

func reconcileGrokRefreshFailure(creds, base *GrokCredentials, rejectedAccess string, force bool, refreshErr error) error {
	return withGrokCredentialsLock(func(path string) error {
		stored, err := readGrokCredentials(path)
		if err == nil && !sameGrokCredentialGeneration(stored, base) {
			if grokGenerationSatisfiesRefresh(stored, rejectedAccess, force) {
				*creds = *stored
				return nil
			}
			// Preserve the generation actually rejected by this call. In
			// particular, do not expose a sibling's rotated-but-unverified
			// checkpoint as though its token had received invalid_grant.
			return fmt.Errorf("Grok credentials changed during failed refresh: %w", refreshErr)
		}
		return refreshErr
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
