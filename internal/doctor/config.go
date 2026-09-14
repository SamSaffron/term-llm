package doctor

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"

	"gopkg.in/yaml.v3"
)

// ConfigCheck inspects config.yaml for keys term-llm ignores, values it cannot
// resolve, and providers that can never authenticate.
type ConfigCheck struct {
	// Path overrides the config file location. Empty uses the default.
	Path string
	// Config is the loaded user config. A nil config is loaded on demand, and
	// a load failure is reported as a finding.
	Config *config.Config
	// ResolveDeferred enables resolution of op://, srv://, file:// and $()
	// values. Resolution runs the same code the config loader runs lazily, so
	// it may execute user-specified commands.
	ResolveDeferred bool
}

// ID implements Check.
func (c *ConfigCheck) ID() string { return "config" }

// Title implements Check.
func (c *ConfigCheck) Title() string { return "Configuration" }

// Run implements Check.
func (c *ConfigCheck) Run(ctx context.Context) []Finding {
	path := c.Path
	if path == "" {
		resolved, err := config.GetConfigPath()
		if err != nil {
			return []Finding{{
				Title:    "cannot locate config file",
				Severity: SeverityError,
				Detail:   err.Error(),
			}}
		}
		path = resolved
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Finding{{
				Title:    "no config file",
				Severity: SeverityInfo,
				Detail:   path + " does not exist; built-in defaults are in use",
			}}
		}
		return []Finding{{
			Title:    "cannot read config file",
			Severity: SeverityError,
			Detail:   err.Error(),
		}}
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return []Finding{{
			Title:    "config file is not valid YAML",
			Severity: SeverityError,
			Detail:   err.Error(),
			Remedy:   "fix the syntax; term-llm is running on defaults until this parses",
		}}
	}

	findings := unknownKeyFindings(path, &root)
	findings = append(findings, deprecatedKeyFindings(&root)...)
	findings = append(findings, c.providerFindings(declaredProviders(&root))...)
	return findings
}

// DeclaredProviderNames returns the providers a config file declares. Loaded
// config always carries every built-in provider because defaults populate the
// map, so only the file distinguishes what the user actually set up.
func DeclaredProviderNames(path string) map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]bool{}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return map[string]bool{}
	}
	return declaredProviders(&root)
}

func declaredProviders(root *yaml.Node) map[string]bool {
	names := map[string]bool{}
	mapping := documentMapping(root)
	if mapping == nil {
		return names
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "providers" {
			continue
		}
		providers := mapping.Content[i+1]
		if providers.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(providers.Content); j += 2 {
			names[providers.Content[j].Value] = true
		}
	}
	return names
}

// unknownKeyFindings reports keys the loader silently ignores. Only the
// outermost unknown key is reported: everything nested under it is ignored for
// the same reason.
func unknownKeyFindings(path string, root *yaml.Node) []Finding {
	unknown := collectUnknownKeys(root)
	findings := make([]Finding, 0, len(unknown))
	for _, key := range unknown {
		detail := fmt.Sprintf("line %d: term-llm ignores this key entirely", key.line)
		remedy := "remove it, or re-run with --fix to comment it out"
		if suggestion := suggestKey(key.path); suggestion != "" {
			detail += fmt.Sprintf("\ndid you mean %q?", suggestion)
		}
		keyPath := key.path
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("unknown config key %q", keyPath),
			Severity: SeverityWarn,
			Detail:   detail,
			Remedy:   remedy,
			Fix: func(ctx context.Context) error {
				return commentOutConfigKey(path, keyPath)
			},
		})
	}
	return findings
}

func deprecatedKeyFindings(root *yaml.Node) []Finding {
	mapping := documentMapping(root)
	if mapping == nil {
		return nil
	}
	var findings []Finding
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "provider" {
			continue
		}
		findings = append(findings, Finding{
			Title:    "deprecated config key \"provider\"",
			Severity: SeverityInfo,
			Detail:   fmt.Sprintf("line %d: still honored as an alias for default_provider", mapping.Content[i].Line),
			Remedy:   "rename it to default_provider",
		})
	}
	return findings
}

// providerFindings validates the providers the user declared, plus the default
// provider, without contacting any network service.
func (c *ConfigCheck) providerFindings(declared map[string]bool) []Finding {
	cfg := c.Config
	if cfg == nil {
		loaded, err := config.Load()
		if err != nil {
			return []Finding{{
				Title:    "config failed validation",
				Severity: SeverityError,
				Detail:   err.Error(),
				Remedy:   "term-llm falls back to defaults while this fails",
			}}
		}
		cfg = loaded
	}

	// Defaults populate every built-in provider, so iterating the loaded map
	// would flag providers the user never asked for.
	selected := map[string]bool{}
	for name := range declared {
		selected[name] = true
	}
	if cfg.DefaultProvider != "" {
		selected[cfg.DefaultProvider] = true
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		if _, known := cfg.Providers[name]; known || declared[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var findings []Finding
	for _, name := range names {
		providerCfg := cfg.Providers[name]
		if err := config.ValidateProviderCompatibility(name, &providerCfg); err != nil {
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("provider %q has an unsupported configuration", name),
				Severity: SeverityError,
				Detail:   err.Error(),
			})
			continue
		}

		findings = append(findings, c.deferredValueFindings(name, providerCfg)...)
		findings = append(findings, unsetEnvFindings(name, providerCfg)...)

		// Bedrock and Ollama authenticate outside term-llm (AWS credential
		// chain, local daemon), so an empty API key says nothing about health.
		switch config.InferProviderType(name, providerCfg.Type) {
		case config.ProviderTypeBedrock, config.ProviderTypeOllama:
			continue
		}
		if source, ok := config.DescribeCredentialSource(name, &providerCfg); !ok {
			// An unused provider block with no key is dead config, not a broken
			// install; only the provider term-llm actually reaches for is a
			// warning.
			severity := SeverityInfo
			if name == cfg.DefaultProvider {
				severity = SeverityWarn
			}
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("provider %q has no usable credential", name),
				Severity: severity,
				Detail:   source,
				Remedy:   "set the API key, or remove the provider block if it is unused",
			})
		}
	}

	if cfg.DefaultProvider != "" {
		if _, configured := cfg.Providers[cfg.DefaultProvider]; !configured && !isBuiltInProvider(cfg.DefaultProvider) {
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("default_provider %q is neither configured nor built in", cfg.DefaultProvider),
				Severity: SeverityError,
				Remedy:   "set default_provider to a provider listed by: term-llm providers",
			})
		}
	}
	return findings
}

func (c *ConfigCheck) deferredValueFindings(name string, providerCfg config.ProviderConfig) []Finding {
	if !c.ResolveDeferred {
		return nil
	}
	values := map[string]string{
		"api_key":  providerCfg.APIKey,
		"base_url": providerCfg.BaseURL,
		"url":      providerCfg.URL,
	}
	for key, value := range providerCfg.Env {
		values["env."+key] = value
	}

	fields := make([]string, 0, len(values))
	for field := range values {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	var findings []Finding
	for _, field := range fields {
		value := values[field]
		if !isDeferredValue(value) {
			continue
		}
		resolved, err := config.ResolveValue(value)
		switch {
		case err != nil:
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("provider %q: %s cannot be resolved", name, field),
				Severity: SeverityError,
				Detail:   fmt.Sprintf("%s: %v", truncateValue(value, 60), err),
				Remedy:   "term-llm fails at request time with this error; fix or remove the value",
			})
		case strings.TrimSpace(resolved) == "":
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("provider %q: %s resolves to an empty value", name, field),
				Severity: SeverityWarn,
				Detail:   truncateValue(value, 60),
			})
		}
	}
	return findings
}

func unsetEnvFindings(name string, providerCfg config.ProviderConfig) []Finding {
	var findings []Finding
	for _, field := range []struct{ label, value string }{
		{"api_key", providerCfg.APIKey},
		{"base_url", providerCfg.BaseURL},
		{"url", providerCfg.URL},
	} {
		variable, ok := envReference(field.value)
		if !ok {
			continue
		}
		if os.Getenv(variable) != "" {
			continue
		}
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("provider %q: %s references unset $%s", name, field.label, variable),
			Severity: SeverityWarn,
			Remedy:   fmt.Sprintf("export %s, or point the value somewhere else", variable),
		})
	}
	return findings
}

func isBuiltInProvider(name string) bool {
	for _, builtin := range config.GetBuiltInProviderNames() {
		if builtin == name {
			return true
		}
	}
	return false
}

// envReference returns the variable name when a value is a bare ${VAR} or $VAR
// reference, matching the loader's expansion rules.
func envReference(value string) (string, bool) {
	value = strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}"):
		return value[2 : len(value)-1], true
	case strings.HasPrefix(value, "$") && !strings.HasPrefix(value, "$("):
		return value[1:], len(value) > 1
	default:
		return "", false
	}
}

// isDeferredValue mirrors the loader's lazy-resolution triggers.
func isDeferredValue(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "op://") ||
		strings.HasPrefix(value, "srv://") ||
		strings.HasPrefix(value, "file://") ||
		(strings.HasPrefix(value, "$(") && strings.HasSuffix(value, ")"))
}

func truncateValue(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit-3] + "..."
}
