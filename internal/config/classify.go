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
	ClassifyProviderTypeSafe   = "typesafe"
	ClassifyProviderOpenAI     = "openai"     // OpenAI Decisions API
	ClassifyProviderCloudflare = "cloudflare" // Cloudflare Workers AI (Clef)
)

// classifyTypeDefaults holds each provider type's built-in settings.
type classifyTypeDefaults struct {
	model          string
	baseURL        string
	timeoutSeconds int
	// keyEnv lists environment fallbacks for api_key, used only with baseURL.
	keyEnv []string
	images bool
	// models lists known models beyond the default, for completion.
	models []string
}

var classifyTypes = map[string]classifyTypeDefaults{
	ClassifyProviderTypeSafe: {
		model: DefaultTypeSafeModel, baseURL: DefaultTypeSafeBaseURL, timeoutSeconds: DefaultTypeSafeTimeoutSeconds,
		keyEnv: []string{"TYPESAFE_API_KEY"},
	},
	ClassifyProviderOpenAI: {
		model: DefaultOpenAIDecisionsModel, baseURL: DefaultOpenAIDecisionsBaseURL, timeoutSeconds: DefaultOpenAIDecisionsTimeoutSeconds,
		keyEnv: []string{"OPENAI_API_KEY"}, images: true,
	},
	ClassifyProviderCloudflare: {
		model: DefaultCloudflareClassifyModel, baseURL: DefaultCloudflareClassifyBaseURL, timeoutSeconds: DefaultCloudflareClassifyTimeoutSeconds,
		keyEnv: []string{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_AUTH_TOKEN"}, images: true,
		models: []string{"clef-flash"},
	},
}

// ClassifyProviderTypes returns the supported provider types, sorted.
func ClassifyProviderTypes() []string {
	types := make([]string, 0, len(classifyTypes))
	for t := range classifyTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// ProviderNames returns configured classification providers plus the built-in providers.
func (c ClassifyConfig) ProviderNames() []string {
	names := ClassifyProviderTypes()
	for name := range c.Providers {
		if !isBuiltinClassifyProvider(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func isBuiltinClassifyProvider(name string) bool {
	_, ok := classifyTypes[name]
	return ok
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
	defaults, ok := classifyTypes[p.Type]
	if !ok {
		return p, fmt.Errorf("classify provider %q has unsupported type %q; set classify.providers.%s.type to one of %s", name, p.Type, name, strings.Join(ClassifyProviderTypes(), ", "))
	}
	if strings.TrimSpace(p.Model) == "" {
		p.Model = defaults.model
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		p.BaseURL = defaults.baseURL
	}
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = defaults.timeoutSeconds
	}
	return p, nil
}

func (p ClassifyProviderConfig) typeDefaults() classifyTypeDefaults {
	if defaults, ok := classifyTypes[strings.TrimSpace(p.Type)]; ok {
		return defaults
	}
	return classifyTypes[ClassifyProviderTypeSafe]
}

// KnownModels returns the built-in models for the provider's type, default
// first.
func (p ClassifyProviderConfig) KnownModels() []string {
	defaults := p.typeDefaults()
	return append([]string{defaults.model}, defaults.models...)
}

// DefaultTimeoutSeconds returns the built-in timeout for the provider's type.
func (p ClassifyProviderConfig) DefaultTimeoutSeconds() int { return p.typeDefaults().timeoutSeconds }

// DefaultBaseURL returns the built-in endpoint for the provider's type.
func (p ClassifyProviderConfig) DefaultBaseURL() string { return p.typeDefaults().baseURL }

// SupportsImages reports whether the provider's type accepts image inputs.
func (p ClassifyProviderConfig) SupportsImages() bool { return p.typeDefaults().images }

// ClassifyKeySpecs expands the canonical provider fields for configured aliases.
// Alias fields have runtime type defaults, not persisted built-in defaults.
func ClassifyKeySpecs(names []string) []KeySpec {
	var specs []KeySpec
	for _, name := range names {
		if name == ClassifyProviderTypeSafe {
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
