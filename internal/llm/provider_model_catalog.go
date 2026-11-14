package llm

import (
	"strings"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
)

// ProviderAllowedModelIDs returns a provider's configured allowed_models
// normalized to provider-facing model IDs. Configured aliases resolve to the
// upstream ID they describe, so discovery can compare them with catalog IDs.
// An unrestricted provider returns nil.
func ProviderAllowedModelIDs(cfg *config.Config, provider string) []string {
	if cfg == nil {
		return nil
	}
	provider = strings.TrimSpace(provider)
	pc, ok := cfg.Providers[provider]
	if !ok || len(pc.AllowedModels) == 0 {
		return nil
	}
	allowed := make([]string, 0, len(pc.AllowedModels))
	seen := make(map[string]bool, len(pc.AllowedModels))
	for _, entry := range pc.AllowedModels {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		entry = config.UpstreamModelForProviderModel(cfg, provider, entry)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		allowed = append(allowed, entry)
	}
	return allowed
}

func providerModelsRestricted(cfg *config.Config, provider string) bool {
	if cfg == nil {
		return false
	}
	return len(cfg.Providers[strings.TrimSpace(provider)].AllowedModels) > 0
}

// filterProviderModelIDs returns only the IDs the provider's allowed_models
// permits. Effort variants of an allowed base remain; an entry that names one
// variant admits only that variant. Unrestricted providers are returned as-is.
func filterProviderModelIDs(cfg *config.Config, provider string, models []string) []string {
	if !providerModelsRestricted(cfg, provider) {
		return models
	}
	filtered := make([]string, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model) == "" {
			continue
		}
		if CheckProviderModelAllowed(cfg, provider, model) == nil {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// mergeProviderAllowedModelIDs appends explicit allowlist entries that the
// source catalog does not already represent, preserving source order. Pinned
// IDs can be absent from an account's cached aliases; discovery still needs a
// usable choice for them.
func mergeProviderAllowedModelIDs(cfg *config.Config, provider string, models []string) []string {
	allowed := ProviderAllowedModelIDs(cfg, provider)
	if len(allowed) == 0 {
		return models
	}
	merged := make([]string, 0, len(models)+len(allowed))
	represented := make(map[string]bool, len(models)+len(allowed))
	for _, model := range models {
		if strings.TrimSpace(model) == "" {
			continue
		}
		merged = append(merged, model)
		represented[config.UpstreamModelForProviderModel(cfg, provider, model)] = true
	}
	for _, model := range allowed {
		if represented[model] {
			continue
		}
		represented[model] = true
		merged = append(merged, model)
	}
	return merged
}

// ProviderModelIDsForDiscovery applies a provider allowlist to a text model
// catalog without expanding effort variants. The web picker exposes efforts
// separately, so it uses this form.
func ProviderModelIDsForDiscovery(cfg *config.Config, provider string, models []string) []string {
	if !providerModelsRestricted(cfg, provider) {
		return models
	}
	return dedupeProviderModelsByUpstream(cfg, provider, mergeProviderAllowedModelIDs(cfg, provider, filterProviderModelIDs(cfg, provider, models)))
}

func dedupeProviderModelsByUpstream(cfg *config.Config, provider string, models []string) []string {
	deduped := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		key := config.UpstreamModelForProviderModel(cfg, provider, model)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, model)
	}
	return deduped
}

// ExpandProviderModelIDsForDiscovery expands effort variants and then applies
// the provider allowlist. Expansion before filtering keeps a base model's
// allowed effort variants, while merging afterwards makes an exact effort-only
// entry discoverable even when its parent catalog entry is not.
func ExpandProviderModelIDsForDiscovery(cfg *config.Config, provider string, models []string) []string {
	expanded := ExpandWithEffortVariantsForProvider(provider, models)
	if !providerModelsRestricted(cfg, provider) {
		return expanded
	}
	filtered := mergeProviderAllowedModelIDs(cfg, provider, filterProviderModelIDs(cfg, provider, expanded))
	return dedupeProviderModelsByUpstream(cfg, provider, filterProviderModelIDs(cfg, provider, ExpandWithEffortVariantsForProvider(provider, filtered)))
}

// ProviderModelsForDiscovery applies a provider allowlist to model metadata
// from an upstream listing, synthesizing entries for explicit allowlist IDs
// that the upstream account cannot currently enumerate.
func ProviderModelsForDiscovery(cfg *config.Config, provider string, models []core.ModelInfo) []core.ModelInfo {
	if !providerModelsRestricted(cfg, provider) {
		return models
	}
	ids := make([]string, 0, len(models))
	byID := make(map[string]core.ModelInfo, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" || CheckProviderModelAllowed(cfg, provider, model.ID) != nil {
			continue
		}
		ids = append(ids, model.ID)
		if _, ok := byID[model.ID]; !ok {
			byID[model.ID] = model
		}
	}
	ids = mergeProviderAllowedModelIDs(cfg, provider, ids)
	merged := make([]core.ModelInfo, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		key := config.UpstreamModelForProviderModel(cfg, provider, id)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		model, ok := byID[id]
		if !ok {
			model = providerModelInfoForAllowedID(cfg, provider, id)
		}
		model.ReasoningEfforts = ProviderReasoningEffortsForDiscovery(cfg, provider, id, model.ReasoningEfforts)
		if effort := model.DefaultReasoningEffort; effort != "" && len(ProviderReasoningEffortsForDiscovery(cfg, provider, id, []string{effort})) == 0 {
			model.DefaultReasoningEffort = ""
		}
		merged = append(merged, model)
	}
	return merged
}

func providerModelInfoForAllowedID(cfg *config.Config, provider, id string) core.ModelInfo {
	info := core.ModelInfo{ID: id}
	if entry, ok := config.ModelConfigForProviderModel(cfg, provider, id); ok {
		info.ReasoningEfforts = append([]string(nil), entry.ReasoningEfforts...)
		info.DefaultReasoningEffort = strings.TrimSpace(entry.DefaultReasoningEffort)
	}
	return info
}

// ProviderReasoningEffortsForDiscovery filters a model's effort metadata to
// variants the provider allowlist admits. An exact effort-only allowlist entry
// keeps that effort without advertising blocked siblings.
func ProviderReasoningEffortsForDiscovery(cfg *config.Config, provider, model string, efforts []string) []string {
	if len(efforts) == 0 || !providerModelsRestricted(cfg, provider) {
		return efforts
	}
	base, _ := BaseModelAndEffortForProvider(provider, model)
	if entry, ok := config.ModelConfigForProviderModel(cfg, provider, model); ok {
		base = trimDeclaredModelEffort(model, entry)
	}
	if strings.TrimSpace(base) == "" {
		return nil
	}
	allowed := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		if CheckProviderModelAllowed(cfg, provider, base+"-"+effort) == nil {
			allowed = append(allowed, effort)
		}
	}
	return allowed
}

// configuredProviderEffortProfileAllowed reports whether the provider-effort
// profile (for example claude-bin-high) resolves to an allowed model.
func configuredProviderEffortProfileAllowed(cfg *config.Config, provider, effort string) bool {
	if cfg == nil || !providerModelsRestricted(cfg, provider) {
		return true
	}
	_, model, effort, configured, supported := parseConfiguredProviderEffort(provider+"-"+effort, cfg)
	if !configured {
		return true
	}
	if !supported {
		return false
	}
	return CheckProviderModelAllowed(cfg, provider, model+"-"+effort) == nil
}
