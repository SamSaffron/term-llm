package config

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
)

// Classification provider types. Built-in provider names match their type.
const (
	ClassifyProviderTypeSafe = "typesafe"
	ClassifyProviderOpenAI   = "openai" // OpenAI Decisions API
)

// ProviderNames returns configured classification providers plus the built-in providers.
func (c ClassifyConfig) ProviderNames() []string {
	names := []string{ClassifyProviderOpenAI, ClassifyProviderTypeSafe}
	for name := range c.Providers {
		if !isBuiltinClassifyProvider(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func isBuiltinClassifyProvider(name string) bool {
	return name == ClassifyProviderTypeSafe || name == ClassifyProviderOpenAI
}

// ResolveProvider selects and validates a provider without resolving credentials.
func (c ClassifyConfig) ResolveProvider(name string) (ClassifyProviderConfig, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(c.DefaultProvider)
	}
	if name == "" {
		name = ClassifyProviderTypeSafe
	}
	p, ok := c.Providers[name]
	if !ok && !isBuiltinClassifyProvider(name) {
		return p, fmt.Errorf("classify provider %q is not configured", name)
	}
	p.Type = strings.TrimSpace(p.Type)
	if p.Type == "" && isBuiltinClassifyProvider(name) {
		p.Type = name
	}
	model, baseURL, timeout := DefaultTypeSafeModel, DefaultTypeSafeBaseURL, DefaultTypeSafeTimeoutSeconds
	switch p.Type {
	case ClassifyProviderTypeSafe:
	case ClassifyProviderOpenAI:
		model, baseURL, timeout = DefaultOpenAIDecisionsModel, DefaultOpenAIDecisionsBaseURL, DefaultOpenAIDecisionsTimeoutSeconds
	default:
		return p, fmt.Errorf("classify provider %q has unsupported type %q; set classify.providers.%s.type to typesafe or openai", name, p.Type, name)
	}
	if strings.TrimSpace(p.Model) == "" {
		p.Model = model
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		p.BaseURL = baseURL
	}
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = timeout
	}
	return p, nil
}

// DefaultModel returns the built-in model for the provider's type.
func (p ClassifyProviderConfig) DefaultModel() string {
	if strings.TrimSpace(p.Type) == ClassifyProviderOpenAI {
		return DefaultOpenAIDecisionsModel
	}
	return DefaultTypeSafeModel
}

// DefaultTimeoutSeconds returns the built-in timeout for the provider's type.
func (p ClassifyProviderConfig) DefaultTimeoutSeconds() int {
	if strings.TrimSpace(p.Type) == ClassifyProviderOpenAI {
		return DefaultOpenAIDecisionsTimeoutSeconds
	}
	return DefaultTypeSafeTimeoutSeconds
}

// DefaultBaseURL returns the built-in endpoint for the provider's type.
func (p ClassifyProviderConfig) DefaultBaseURL() string {
	if strings.TrimSpace(p.Type) == ClassifyProviderOpenAI {
		return DefaultOpenAIDecisionsBaseURL
	}
	return DefaultTypeSafeBaseURL
}

// ClassifyKeySpecs expands the canonical provider fields for configured aliases.
// Alias fields have runtime type defaults, not persisted built-in defaults.
func ClassifyKeySpecs(names []string) []KeySpec {
	var specs []KeySpec
	for _, name := range names {
		if name == "typesafe" {
			continue
		}
		for _, spec := range ConfigKeySpecs() {
			const prefix = "classify.providers.typesafe."
			if !strings.HasPrefix(spec.Path, prefix) {
				continue
			}
			spec.Path = "classify.providers." + name + "." + strings.TrimPrefix(spec.Path, prefix)
			spec.HasDefault = false
			spec.Default = nil
			specs = append(specs, spec)
		}
	}
	return specs
}

// Validate rejects unusable live classifier thresholds, including non-finite values.
func (c LiveClassifyConfig) Validate() error {
	for _, threshold := range []struct {
		name  string
		value float64
	}{
		{"status", c.MinConfidence.Status},
		{"new_session", c.MinConfidence.NewSession},
		{"switch_session", c.MinConfidence.SwitchSession},
		{"steer_now", c.MinConfidence.SteerNow},
		{"side", c.MinConfidence.Side},
	} {
		if math.IsNaN(threshold.value) || math.IsInf(threshold.value, 0) || threshold.value < 0 || threshold.value > 1 {
			return fmt.Errorf("live.classify.min_confidence.%s must be between 0 and 1", threshold.name)
		}
	}
	return nil
}

// Validate rejects unusable Guardian confidence thresholds, including non-finite values.
func (c GuardianClassifyConfig) Validate() error {
	if math.IsNaN(c.MinConfidence) || math.IsInf(c.MinConfidence, 0) || c.MinConfidence < 0 || c.MinConfidence > 1 {
		return fmt.Errorf("guardian.classify.min_confidence must be between 0 and 1")
	}
	return nil
}

// Enabled reports whether the classify fallback selects an LLM reviewer.
func (c GuardianFallbackConfig) Enabled() bool {
	return strings.TrimSpace(c.Provider) != "" || strings.TrimSpace(c.Model) != ""
}

// Validate rejects fallback settings that cannot take effect on the configured backend.
func (c GuardianFallbackConfig) Validate(backend string) error {
	enabled := c.Enabled()
	hasLogPath := strings.TrimSpace(c.LogPath) != ""
	if hasLogPath && !enabled {
		return fmt.Errorf("guardian.fallback.log_path requires guardian.fallback.provider or guardian.fallback.model")
	}
	if (enabled || hasLogPath) && isLLMGuardianBackend(backend) {
		return fmt.Errorf("guardian.fallback requires guardian.backend: classify")
	}
	// Escalation records contain transcript evidence, so a relative path that
	// would land in the process working directory is rejected here rather than
	// silently disabling logging at runtime. A `~` prefix is expanded later.
	if logPath := strings.TrimSpace(c.LogPath); hasLogPath && !strings.EqualFold(logPath, "off") && !strings.HasPrefix(logPath, "~") && !filepath.IsAbs(logPath) {
		return fmt.Errorf("guardian.fallback.log_path must be absolute, start with ~, or be off (got %q)", logPath)
	}
	return nil
}

// isLLMGuardianBackend reports whether a Guardian backend value means the
// default LLM reviewer, i.e. anything that is not the classify backend.
func isLLMGuardianBackend(backend string) bool {
	backend = strings.TrimSpace(backend)
	return backend == "" || backend == "llm"
}
