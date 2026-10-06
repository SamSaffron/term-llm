package llm

import (
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
)

// Provider availability answers one question for every provider picker (web
// UI, `term-llm providers`, chat /model): is this provider configured? A
// provider is configured when it is named in config.yaml, is the
// default_provider, has an enabling environment variable, or has local login
// state — unless providers.<name>.enabled is false.

// Reasons returned by ProviderConfiguredVia.
const (
	ConfiguredViaConfig  = "config"
	ConfiguredViaDefault = "default"
	ConfiguredViaEnv     = "env"
	ConfiguredViaLogin   = "login"
)

// ProviderHasLocalCredentials reports whether a built-in provider has local
// login state: term-llm OAuth credentials on disk or a signed-in companion
// CLI. Some probes stat files and (agy on macOS) spawn a helper, so pickers
// should go through a ProviderCredentialCache.
func ProviderHasLocalCredentials(name string) bool {
	switch name {
	case "claude-bin":
		// Claude Code keeps its login in the macOS keychain or its own config;
		// an installed CLI is the cheap, reliable signal.
		_, err := exec.LookPath("claude")
		return err == nil
	case "chatgpt":
		return credentials.ChatGPTCredentialsExist()
	case "grok":
		return credentials.GrokCredentialsExist()
	case "copilot":
		return credentials.CopilotCredentialsExist()
	case "grok-bin":
		path := grokBinAuthPath(os.Getenv("GROK_AUTH_PATH"))
		_, err := os.Stat(path)
		return path != "" && err == nil
	case "cursor-bin":
		return CursorBinHasCredentials()
	case "agy-bin":
		return AgyBinHasCredentials()
	}
	return false
}

// ProviderConfiguredVia reports why provider name is configured (one of the
// ConfiguredVia* constants), or "" when it is not configured or is disabled.
// hasLocalCredentials detects local login state for built-ins; pass a cached
// detector such as ProviderCredentialCache.Has.
func ProviderConfiguredVia(cfg *config.Config, name string, hasLocalCredentials func(string) bool) string {
	if cfg.ProviderDisabled(name) {
		return ""
	}
	if cfg != nil {
		if pc, ok := cfg.Providers[name]; ok && !pc.FromDefaults {
			return ConfiguredViaConfig
		}
		if cfg.DefaultProvider == name {
			return ConfiguredViaDefault
		}
	}
	spec, ok := config.BuiltinProvider(name)
	if !ok {
		return ""
	}
	if spec.EnabledByEnv() != "" {
		return ConfiguredViaEnv
	}
	if hasLocalCredentials != nil && hasLocalCredentials(name) {
		return ConfiguredViaLogin
	}
	return ""
}

// ProviderCredentialCacheTTL bounds how long a sign-in or sign-out of a
// built-in provider takes to show up in provider pickers.
const ProviderCredentialCacheTTL = 30 * time.Second

// ProviderCredentialCache memoizes ProviderHasLocalCredentials for every
// built-in provider so pickers do not run the probes on every request. The
// zero value is ready to use.
type ProviderCredentialCache struct {
	// Detect overrides the probe (tests); nil uses ProviderHasLocalCredentials.
	Detect func(string) bool
	// Now overrides the clock (tests); nil uses time.Now.
	Now func() time.Time

	mu     sync.Mutex
	at     time.Time
	values map[string]bool
}

func (c *ProviderCredentialCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// snapshot returns local-credential presence for every built-in provider,
// refreshing it when stale. Concurrent callers share one refresh.
func (c *ProviderCredentialCache) snapshot() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values != nil && c.now().Sub(c.at) < ProviderCredentialCacheTTL {
		return c.values
	}
	detect := c.Detect
	if detect == nil {
		detect = ProviderHasLocalCredentials
	}
	values := make(map[string]bool)
	for _, name := range GetBuiltInProviderNames() {
		values[name] = detect(name)
	}
	c.values = values
	c.at = c.now()
	return values
}

// Has reports whether the built-in provider name has local credentials.
func (c *ProviderCredentialCache) Has(name string) bool {
	return c.snapshot()[name]
}

// Warm refreshes the cache in the background so the first picker does not pay
// for the probes, without delaying startup.
func (c *ProviderCredentialCache) Warm() {
	go c.snapshot()
}

// DefaultProviderCredentials is the process-wide cache used by pickers that
// do not own one.
var DefaultProviderCredentials ProviderCredentialCache
