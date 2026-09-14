package doctor

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
)

// CredentialsCheck inspects stored OAuth credentials without contacting any
// provider. It reports permissions, parse failures, and expiry that cannot be
// refreshed automatically.
type CredentialsCheck struct {
	// Config is the loaded user config. A nil config is loaded on demand and
	// only affects whether missing credentials are informational or a warning.
	Config *config.Config
	// Declared names the providers the config file sets up. Loaded config
	// carries every built-in provider, so without this a missing credential
	// would warn for providers the user never configured. A nil map falls back
	// to the loaded provider map.
	Declared map[string]bool
}

// ID implements Check.
func (c *CredentialsCheck) ID() string { return "credentials" }

// Title implements Check.
func (c *CredentialsCheck) Title() string { return "Credentials" }

type credentialState struct {
	expiresAt  int64
	hasRefresh bool
	accountID  string
}

type credentialSource struct {
	provider string
	path     func() (string, error)
	load     func() (credentialState, error)
}

func credentialSources() []credentialSource {
	return []credentialSource{
		{
			provider: "chatgpt",
			path:     credentials.ChatGPTCredentialsPath,
			load: func() (credentialState, error) {
				creds, err := credentials.GetChatGPTCredentials()
				if err != nil {
					return credentialState{}, err
				}
				return credentialState{expiresAt: creds.ExpiresAt, hasRefresh: creds.RefreshToken != "", accountID: creds.AccountID}, nil
			},
		},
		{
			provider: "grok",
			path:     credentials.GrokCredentialsPath,
			load: func() (credentialState, error) {
				creds, err := credentials.GetGrokCredentials()
				if err != nil {
					return credentialState{}, err
				}
				return credentialState{expiresAt: creds.ExpiresAt, hasRefresh: creds.RefreshToken != "", accountID: creds.AccountID}, nil
			},
		},
		{
			provider: "copilot",
			path:     credentials.CopilotCredentialsPath,
			load: func() (credentialState, error) {
				creds, err := credentials.GetCopilotCredentials()
				if err != nil {
					return credentialState{}, err
				}
				return credentialState{expiresAt: creds.ExpiresAt}, nil
			},
		},
	}
}

// Run implements Check.
func (c *CredentialsCheck) Run(ctx context.Context) []Finding {
	cfg := c.Config
	if cfg == nil {
		cfg, _ = config.Load()
	}

	var findings []Finding
	for _, source := range credentialSources() {
		findings = append(findings, credentialFindings(source, cfg, c.Declared)...)
	}
	return findings
}

func credentialFindings(source credentialSource, cfg *config.Config, declared map[string]bool) []Finding {
	path, err := source.path()
	if err != nil {
		return []Finding{{
			Title:    fmt.Sprintf("%s: cannot locate credential file", source.provider),
			Severity: SeverityError,
			Detail:   err.Error(),
		}}
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			severity := SeverityInfo
			if providerInUse(cfg, declared, source.provider) {
				severity = SeverityWarn
			}
			return []Finding{{
				Title:    fmt.Sprintf("%s: not signed in", source.provider),
				Severity: severity,
				Detail:   path + " does not exist",
				Remedy:   "run: term-llm auth login " + source.provider,
			}}
		}
		return []Finding{{
			Title:    fmt.Sprintf("%s: cannot stat credential file", source.provider),
			Severity: SeverityError,
			Detail:   err.Error(),
		}}
	}

	var findings []Finding
	if finding, ok := permissionFinding(source.provider, path, info); ok {
		findings = append(findings, finding)
	}

	state, err := source.load()
	if err != nil {
		return append(findings, Finding{
			Title:    fmt.Sprintf("%s: credential file is unusable", source.provider),
			Severity: SeverityError,
			Detail:   err.Error(),
			Remedy:   "run: term-llm auth login " + source.provider,
		})
	}

	return append(findings, expiryFinding(source.provider, state))
}

// permissionFinding flags credential files readable by other users. Windows
// file modes do not carry POSIX permission bits, so the check is skipped there.
func permissionFinding(provider, path string, info os.FileInfo) (Finding, bool) {
	if runtime.GOOS == "windows" {
		return Finding{}, false
	}
	mode := info.Mode().Perm()
	if mode&0o077 == 0 {
		return Finding{}, false
	}
	return Finding{
		Title:    fmt.Sprintf("%s: credential file is readable by other users", provider),
		Severity: SeverityWarn,
		Detail:   fmt.Sprintf("%s has mode %04o, want 0600", path, mode),
		Remedy:   "restrict it with --fix, or run: chmod 600 " + path,
		Fix: func(ctx context.Context) error {
			return os.Chmod(path, 0o600)
		},
	}, true
}

func expiryFinding(provider string, state credentialState) Finding {
	if state.expiresAt == 0 {
		return Finding{
			Title:    fmt.Sprintf("%s: signed in", provider),
			Severity: SeverityOK,
			Detail:   "token carries no expiry",
		}
	}
	expiry := time.Unix(state.expiresAt, 0).UTC()
	if time.Now().Before(expiry) {
		return Finding{
			Title:    fmt.Sprintf("%s: signed in", provider),
			Severity: SeverityOK,
			Detail:   fmt.Sprintf("expires %s", expiry.Format(time.RFC3339)),
		}
	}
	if state.hasRefresh {
		return Finding{
			Title:    fmt.Sprintf("%s: access token expired", provider),
			Severity: SeverityInfo,
			Detail:   fmt.Sprintf("expired %s; a refresh token is stored so the next request renews it", expiry.Format(time.RFC3339)),
		}
	}
	return Finding{
		Title:    fmt.Sprintf("%s: access token expired and cannot refresh", provider),
		Severity: SeverityWarn,
		Detail:   fmt.Sprintf("expired %s with no refresh token", expiry.Format(time.RFC3339)),
		Remedy:   "run: term-llm auth login " + provider,
	}
}

// providerInUse reports whether a provider is configured or selected by default,
// which turns a missing credential from a note into a warning.
func providerInUse(cfg *config.Config, declared map[string]bool, provider string) bool {
	if cfg == nil {
		return false
	}
	if cfg.DefaultProvider == provider {
		return true
	}
	if declared != nil {
		return declared[provider]
	}
	_, configured := cfg.Providers[provider]
	return configured
}
