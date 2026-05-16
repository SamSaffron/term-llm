package llm

import "strings"

const (
	// ServiceTierFast is Codex/ChatGPT's API value for user-facing "fast" mode.
	ServiceTierFast = "priority"
	// ServiceTierFastAlias is the legacy/user-facing alias accepted in config and requests.
	ServiceTierFastAlias = "fast"
)

// ModelServiceTier describes an optional service tier advertised by a model catalog.
type ModelServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// NormalizeServiceTier maps user-facing aliases to API values. Unknown values are
// returned trimmed so future service tiers can pass through when supported.
func NormalizeServiceTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case ServiceTierFastAlias, ServiceTierFast:
		return ServiceTierFast
	default:
		return strings.TrimSpace(value)
	}
}

// ModelSupportsServiceTier reports whether model metadata advertises tier.
func ModelSupportsServiceTier(model ModelInfo, tier string) bool {
	tier = NormalizeServiceTier(tier)
	if tier == "" {
		return false
	}
	for _, serviceTier := range model.ServiceTiers {
		if NormalizeServiceTier(serviceTier.ID) == tier {
			return true
		}
	}
	return false
}

// ModelSupportsFast reports whether model metadata advertises fast mode.
func ModelSupportsFast(model ModelInfo) bool {
	if ModelSupportsServiceTier(model, ServiceTierFast) {
		return true
	}
	for _, speedTier := range model.AdditionalSpeedTiers {
		if NormalizeServiceTier(speedTier) == ServiceTierFast {
			return true
		}
	}
	return false
}
