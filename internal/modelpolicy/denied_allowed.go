package modelpolicy

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

// deniedAllowedForConfig narrows the entries advertised by a denial to the
// intersection with each provider's allowed_models. An entry that names a
// provider wildcard expands to the provider's explicit allowlist; an entry
// whose own model is blocked is replaced by the allowlisted variants it still
// covers. This keeps the error from advertising models enforcement will reject.
func deniedAllowedForConfig(cfg *config.Config, allowed []string) []string {
	if cfg == nil {
		return append([]string(nil), allowed...)
	}
	out := make([]string, 0, len(allowed))
	seen := make(map[string]bool, len(allowed))
	appendEntry := func(entry string) {
		if entry == "" || seen[entry] {
			return
		}
		seen[entry] = true
		out = append(out, entry)
	}
	for _, entry := range allowed {
		entry = strings.TrimSpace(entry)
		provider, model, found := strings.Cut(entry, ":")
		if !found || provider == "" || model == "" {
			appendEntry(entry)
			continue
		}
		providerAllowed := llm.ProviderAllowedModelIDs(cfg, provider)
		switch {
		case model == "*" && len(providerAllowed) == 0:
			appendEntry(entry)
		case model == "*":
			for _, candidate := range providerAllowed {
				appendEntry(provider + ":" + candidate)
			}
		case llm.CheckProviderModelAllowed(cfg, provider, model) == nil:
			appendEntry(entry)
		default:
			for _, candidate := range providerAllowed {
				if llm.IsModelOrEffortVariantForConfig(cfg, provider, candidate, model) {
					appendEntry(provider + ":" + candidate)
				}
			}
		}
	}
	if len(out) == 0 {
		// Every advertised rule entry is blocked by a provider allowlist.
		return []string{"none (providers' allowed_models exclude the agent list)"}
	}
	return out
}
