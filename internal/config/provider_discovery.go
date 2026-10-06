package config

import (
	"fmt"
	"strings"
)

// Provider discovery answers one question for every text-provider picker and
// for provider creation: is this provider enabled? Under the default
// provider_discovery (auto) a provider is enabled when it is named in
// config.yaml, is the default_provider, has an enabling environment variable,
// or has local login state. provider_discovery: env keeps only config.yaml
// blocks and environment variables; provider_discovery: config keeps only
// config.yaml blocks. providers.<name>.enabled: false always wins.

// Provider discovery modes for Config.ProviderDiscovery.
const (
	ProviderDiscoveryAuto   = "auto"
	ProviderDiscoveryEnv    = "env"
	ProviderDiscoveryConfig = "config"
)

// Reasons returned by ProviderConfiguredVia.
const (
	ConfiguredViaConfig  = "config"
	ConfiguredViaDefault = "default"
	ConfiguredViaEnv     = "env"
	ConfiguredViaLogin   = "login"
)

// ProviderDiscoveryMode returns the normalized provider discovery mode; an
// unset value is auto.
func (c *Config) ProviderDiscoveryMode() string {
	if c == nil {
		return ProviderDiscoveryAuto
	}
	mode := strings.ToLower(strings.TrimSpace(c.ProviderDiscovery))
	if mode == "" {
		return ProviderDiscoveryAuto
	}
	return mode
}

// ValidateProviderDiscovery rejects unknown provider_discovery values, so a
// typo cannot silently fall back to opportunistic discovery.
func (c *Config) ValidateProviderDiscovery() error {
	switch c.ProviderDiscoveryMode() {
	case ProviderDiscoveryAuto, ProviderDiscoveryEnv, ProviderDiscoveryConfig:
		return nil
	}
	return fmt.Errorf("invalid provider_discovery %q: expected auto, env, or config", c.ProviderDiscovery)
}

// ProviderDeclared reports whether config.yaml has a providers.<name> block.
// Entries that exist only because of schema defaults or runtime model
// overrides do not count.
func (c *Config) ProviderDeclared(name string) bool {
	if c == nil {
		return false
	}
	pc, ok := c.Providers[name]
	return ok && !pc.FromDefaults
}

// ProviderConfiguredVia reports why provider name is enabled (one of the
// ConfiguredVia* constants), or "" when it is not enabled or is disabled.
// hasLocalCredentials detects local login state for built-ins and is only
// consulted under provider_discovery: auto.
func (c *Config) ProviderConfiguredVia(name string, hasLocalCredentials func(string) bool) string {
	if c.ProviderDisabled(name) {
		return ""
	}
	if c.ProviderDeclared(name) {
		return ConfiguredViaConfig
	}
	mode := c.ProviderDiscoveryMode()
	if mode == ProviderDiscoveryConfig {
		return ""
	}
	// default_provider is only a selector outside auto: runtime overrides
	// (-p, agents) rewrite it, so it cannot be trusted to enable a provider.
	if mode == ProviderDiscoveryAuto && c != nil && c.DefaultProvider == name {
		return ConfiguredViaDefault
	}
	spec, ok := BuiltinProvider(name)
	if !ok {
		return ""
	}
	if spec.EnabledByEnv() != "" {
		return ConfiguredViaEnv
	}
	if mode == ProviderDiscoveryAuto && hasLocalCredentials != nil && hasLocalCredentials(name) {
		return ConfiguredViaLogin
	}
	return ""
}

// ProviderEnableHint returns the step that enables provider name under a
// non-auto provider_discovery mode.
func ProviderEnableHint(mode, name string) string {
	configHint := "add providers." + name + " to config.yaml"
	if spec, ok := BuiltinProvider(name); ok && mode == ProviderDiscoveryEnv {
		if env := spec.EnablingEnv(); len(env) > 0 {
			return "set " + strings.Join(env, " or ") + ", or " + configHint
		}
	}
	return configHint
}

// ProviderNotEnabledError reports a provider that provider_discovery does not
// enable, or nil when it may be used. Under auto any provider may be selected
// explicitly. It never runs local login probes and ignores
// providers.<name>.enabled: false, which callers report separately.
func (c *Config) ProviderNotEnabledError(name string) error {
	mode := c.ProviderDiscoveryMode()
	if mode == ProviderDiscoveryAuto || name == "debug" {
		return nil
	}
	if c.ProviderDeclared(name) || c.ProviderConfiguredVia(name, nil) != "" {
		return nil
	}
	if spec, ok := BuiltinProvider(name); ok && mode == ProviderDiscoveryEnv && len(spec.EnablingEnv()) > 0 {
		return fmt.Errorf("provider %q is not enabled: provider_discovery is %q and neither providers.%s in config.yaml nor $%s is set (add \"%s: {}\" under providers: to enable it)",
			name, mode, name, strings.Join(spec.EnablingEnv(), " / $"), name)
	}
	return fmt.Errorf("provider %q is not enabled: provider_discovery is %q and config.yaml has no providers.%s block (add \"%s: {}\" under providers: to enable it)",
		name, mode, name, name)
}
