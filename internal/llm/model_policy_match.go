package llm

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// IsKnownProviderModel reports whether the provider has catalog or configured
// metadata for this literal ID. Unknown model IDs can use conservative effort
// suffix matching, but catalog IDs must be checked against their capabilities.
func IsKnownProviderModel(provider, model string) bool {
	for _, entry := range resolveProviderModelEntries(provider) {
		if entry.ID == model {
			return true
		}
	}
	if _, _, efforts := configBaseModelAndEffortForProvider(provider, model); len(efforts) > 0 {
		return true
	}
	for _, id := range ProviderModelIDs(provider) {
		if id == model {
			return true
		}
	}
	return false
}

// IsModelOrEffortVariant matches a selected model against an allowlisted base
// and only accepts suffixes that represent reasoning efforts, never arbitrary
// model names such as -mini or -spark.
func IsModelOrEffortVariant(provider, selected, base string) bool {
	if selected == base {
		return true
	}
	if base == "" || selected == "" {
		return false
	}
	// An explicitly suffixed rule is a literal model ID, not a family.
	for _, suffix := range knownEffortSuffixes {
		if strings.HasSuffix(base, "-"+suffix) && !IsKnownProviderModel(provider, base) {
			return false
		}
	}
	if parsed, effort := BaseModelAndEffortForProvider(provider, base); effort != "" && parsed != base {
		return false
	}
	if parsed, effort := BaseModelAndEffortForProvider(provider, selected); effort != "" && parsed == base {
		// Parsing can use provider defaults; for known IDs the catalog is
		// authoritative, including explicit empty effort lists.
		if !IsKnownProviderModel(provider, base) {
			return true
		}
		for _, allowed := range ReasoningEffortsForProviderModel(provider, base) {
			if selected == base+"-"+allowed || (effort == "max" && selected == base+"-ultra") {
				return true
			}
		}
		return false
	}
	if IsKnownProviderModel(provider, base) {
		for _, effort := range ReasoningEffortsForProviderModel(provider, base) {
			if selected == base+"-"+effort {
				return true
			}
		}
		return false
	}
	for _, effort := range knownEffortSuffixes {
		if selected == base+"-"+effort {
			return true
		}
	}
	return false
}

// IsModelOrEffortVariantForConfig applies configured model IDs and aliases as
// catalog facts before considering an unknown-ID suffix heuristic.
func IsModelOrEffortVariantForConfig(cfg *config.Config, provider, selected, base string) bool {
	if selected == base {
		return true
	}
	if cfg != nil {
		if pc, ok := cfg.Providers[provider]; ok {
			for _, id := range pc.Models {
				if id == base {
					return configuredEffortVariant(cfg, provider, selected, base)
				}
			}
			for _, entry := range pc.ModelConfigs {
				if entry.ID == base || entry.DisplayName() == base {
					return configuredEffortVariant(cfg, provider, selected, base)
				}
			}
		}
	}
	return IsModelOrEffortVariant(provider, selected, base)
}

func configuredEffortVariant(cfg *config.Config, provider, selected, base string) bool {
	entry, ok := config.ModelConfigForProviderModel(cfg, provider, base)
	if ok {
		for _, effort := range entry.ReasoningEfforts {
			if selected == base+"-"+effort {
				return true
			}
		}
		return false
	}
	if IsKnownProviderModel(provider, base) {
		return IsModelOrEffortVariant(provider, selected, base)
	}
	return false
}
