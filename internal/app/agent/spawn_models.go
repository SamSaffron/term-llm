package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/samsaffron/term-llm/internal/app/providers"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

// spawnModelCredentials memoizes the local login probes behind provider
// discovery so validating model overrides stays cheap.
var spawnModelCredentials llm.ProviderCredentialCache

// SpawnModelCatalog answers which providers a spawn_agent model override may
// name, using the same discovery rules as the web UI model picker. A runner
// embeds it to satisfy spawnmodel.Catalog.
type SpawnModelCatalog struct {
	cfg *config.Config
	// hasLocalCredentials overrides the login probe (tests); nil uses the
	// shared cache.
	hasLocalCredentials func(string) bool
}

// NewSpawnModelCatalog returns a catalog over cfg.
func NewSpawnModelCatalog(cfg *config.Config) SpawnModelCatalog {
	return SpawnModelCatalog{cfg: cfg}
}

// spawnModelProviders lists the providers this host enables for validation
// and recovery guidance. It is never called while generating the tool schema.
func (c SpawnModelCatalog) spawnModelProviders() []string {
	if c.cfg == nil {
		return nil
	}
	has := c.hasLocalCredentials
	if has == nil {
		has = spawnModelCredentials.Has
	}
	var names []string
	for _, p := range providers.BuildListWith(c.cfg, has) {
		if p.Configured {
			names = append(names, p.Name)
		}
	}
	return names
}

// ValidateSpawnModel rejects a provider:model override whose provider this
// host does not enable. ApplyModelOverride treats an unknown prefix as part of
// a plain model name (for colon-tagged local models), so without this check a
// guessed prefix such as "codex:" or "expensive:" reaches the default
// provider's API and the child fails there.
func (c SpawnModelCatalog) ValidateSpawnModel(model string) error {
	if c.cfg == nil {
		return nil
	}
	enabled := c.spawnModelProviders()
	provider, _, err := llm.ParseProviderModel(model, c.cfg)
	if err == nil && !c.cfg.ProviderDisabled(provider) && (provider == "debug" || slices.Contains(enabled, provider)) {
		return nil
	}
	name, _, _ := strings.Cut(model, ":")
	reason := fmt.Sprintf("provider %q is not enabled on this host", name)
	if err != nil {
		reason = fmt.Sprintf("unknown provider %q", name)
	}
	if len(enabled) == 0 {
		return fmt.Errorf("model %q: %s; omit model to use the agent's default", model, reason)
	}
	suggestions, example := spawnModelSuggestions(enabled, c.cfg.DefaultProvider)
	return fmt.Errorf("model %q: %s; use one of %s (for example %q), or omit model to use the agent's default",
		model, reason, suggestions, example+":<model>")
}

const maxSpawnProviderSuggestions = 3

// spawnModelSuggestions samples a non-empty provider list, preferring the
// enabled default. Never enumerate models: a provider can have hundreds.
func spawnModelSuggestions(enabled []string, preferred string) (string, string) {
	names := make([]string, 0, maxSpawnProviderSuggestions)
	if slices.Contains(enabled, preferred) {
		names = append(names, preferred)
	}
	for _, name := range enabled {
		if len(names) == maxSpawnProviderSuggestions {
			break
		}
		if name != preferred {
			names = append(names, name)
		}
	}
	suggestions := strings.Join(names, ", ")
	if remaining := len(enabled) - len(names); remaining > 0 {
		suggestions += fmt.Sprintf(" (and %d more; run `term-llm providers --configured` for the full list)", remaining)
	}
	return suggestions, names[0]
}
